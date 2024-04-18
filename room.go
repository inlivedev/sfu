package sfu

import (
	"context"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
)

const (
	StateRoomOpen       = "open"
	StateRoomClosed     = "closed"
	EventRoomClosed     = "room_closed"
	EventRoomClientLeft = "room_client_left"
)

type Options struct {
	EnableMux                bool
	PortStart                uint16
	PortEnd                  uint16
	WebRTCPort               int
	EnableBridging           bool
	EnableBandwidthEstimator bool
	IceServers               []webrtc.ICEServer
	PublicIP                 string
	// If PublicIP is set, then you should consider to set this NAT1To1IPsCandidateType as well. By default, it is set to ICECandidateTypeHost.
	// Two types of candidates are supported:
	//
	// ICECandidateTypeHost:
	//
	//	The public IP address will be used for the host candidate in the SDP.
	//
	// ICECandidateTypeSrflx:
	//
	//	A server reflexive candidate with the given public IP address will be added to the SDP.
	//
	// Please note that if you choose ICECandidateTypeHost, then the private IP address
	// won't be advertised with the peer. Also, this option cannot be used along with mDNS.
	//
	// If you choose ICECandidateTypeSrflx, it simply adds a server reflexive candidate
	// with the public IP. The host candidate is still available along with mDNS
	// capabilities unaffected. Also, you cannot give STUN server URL at the same time.
	// It will result in an error otherwise.
	NAT1To1IPsCandidateType webrtc.ICECandidateType
	MinPlayoutDelay         uint16
	MaxPlayoutDelay         uint16
}

func DefaultOptions() Options {
	return Options{
		PortStart:                49152,
		PortEnd:                  65535,
		EnableMux:                false,
		WebRTCPort:               50005,
		EnableBandwidthEstimator: true,
		IceServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
		PublicIP:                "",
		NAT1To1IPsCandidateType: webrtc.ICECandidateTypeHost,
		MinPlayoutDelay:         100,
		MaxPlayoutDelay:         100,
	}
}

type Event struct {
	Type string
	Time time.Time
	Data map[string]interface{}
}

type Room struct {
	onRoomClosedCallbacks   []func(id string)
	onClientJoinedCallbacks []func(*Client)
	onClientLeftCallbacks   []func(*Client)
	context                 context.Context
	cancel                  context.CancelFunc
	id                      string
	token                   string
	RenegotiationChan       map[string]chan bool
	name                    string
	mu                      *sync.RWMutex
	meta                    *Metadata
	sfu                     *SFU
	state                   string
	stats                   map[string]TrackStats
	kind                    string
	extensions              []IExtension
	OnEvent                 func(event Event)
	options                 RoomOptions
}

type RoomOptions struct {
	// Configures the bitrates configuration that will be used by the room
	// Make sure to use the same bitrate config when publishing video because this is used to manage the usage bandwidth in this room
	Bitrates BitrateConfigs `json:"bitrates"`
	// Configures the codecs that will be used by the room
	Codecs []string `json:"codecs" enums:"video/VP9,video/H264,video/VP8,audio/red,audio/opus" example:"video/VP9,video/H264,video/VP8,audio/red,audio/opus"`
	// Configures the interval in nanoseconds of sending PLIs to clients that will generate keyframe, default is 0 means it will use auto PLI request only when needed.
	// More often means more bandwidth usage but more stability on video quality when packet loss, but client libs supposed to request PLI automatically when needed.
	PLIInterval time.Duration `json:"pli_interval_ns" example:"0"`
	// Configure the mapping of spatsial and temporal layers to quality level
	// Use this to use scalable video coding (SVC) to control the bitrate level of the video
	QualityPresets QualityPresets `json:"quality_presets"`
	// Configure the timeout in nanonseconds when the room is empty it will close after the timeout exceeded. Default is 5 minutes
	EmptyRoomTimeout time.Duration `json:"empty_room_timeout_ns" example:"300000000000" default:"300000000000"`
}

func DefaultRoomOptions() RoomOptions {
	return RoomOptions{
		Bitrates:         DefaultBitrates(),
		QualityPresets:   DefaultQualityPresets(),
		Codecs:           []string{webrtc.MimeTypeVP9, webrtc.MimeTypeH264, webrtc.MimeTypeVP8, "audio/red", webrtc.MimeTypeOpus},
		PLIInterval:      0,
		EmptyRoomTimeout: 3 * time.Minute,
	}
}

func newRoom(id, name string, sfu *SFU, kind string, opts RoomOptions) *Room {
	localContext, cancel := context.WithCancel(sfu.context)

	room := &Room{
		id:         id,
		context:    localContext,
		cancel:     cancel,
		sfu:        sfu,
		token:      GenerateID(21),
		stats:      make(map[string]TrackStats),
		state:      StateRoomOpen,
		name:       name,
		mu:         &sync.RWMutex{},
		meta:       NewMetadata(),
		extensions: make([]IExtension, 0),
		kind:       kind,
		options:    opts,
	}

	sfu.OnClientRemoved(func(client *Client) {
		room.onClientLeft(client)
	})

	go room.loopRecordStats()

	return room
}

func (r *Room) ID() string {
	return r.id
}

func (r *Room) Name() string {
	return r.name
}

func (r *Room) Kind() string {
	return r.kind
}

func (r *Room) AddExtension(extension IExtension) {
	r.extensions = append(r.extensions, extension)
}

// Close the room and stop all clients. All connected clients will stopped and removed from the room.
// All clients will get `connectionstateevent` with `closed` state.
// https://developer.mozilla.org/en-US/docs/Web/API/RTCPeerConnection/connectionstatechange_event
func (r *Room) Close() error {
	if r.state == StateRoomClosed {
		return ErrRoomIsClosed
	}

	r.cancel()

	r.sfu.Stop()

	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, callback := range r.onRoomClosedCallbacks {
		callback(r.id)
	}

	r.state = StateRoomClosed

	return nil
}

// Stopping client is async, it will just stop the client and return immediately
// You should use OnClientLeft to get notified when the client is actually stopped
func (r *Room) StopClient(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var client *Client

	var err error

	if client, err = r.sfu.GetClient(id); err != nil {
		return err
	}

	return client.stop()
}

func (r *Room) AddClient(id, name string, opts ClientOptions) (*Client, error) {
	if r.state == StateRoomClosed {
		return nil, ErrRoomIsClosed
	}

	for _, ext := range r.extensions {
		if err := ext.OnBeforeClientAdded(r, id); err != nil {
			return nil, err
		}
	}

	client, _ := r.sfu.GetClient(id)
	if client != nil {
		return nil, ErrClientExists
	}

	client = r.sfu.NewClient(id, name, opts)

	// stop client if not connecting for a specific time
	initConnection := true
	go func() {
		timeout, cancel := context.WithTimeout(client.context, opts.IdleTimeout)
		defer cancel()

		mu := sync.Mutex{}

		connectingChan := make(chan bool)

		timeoutReached := false

		client.OnConnectionStateChanged(func(state webrtc.PeerConnectionState) {
			mu.Lock()
			defer mu.Unlock()

			if initConnection && state == webrtc.PeerConnectionStateConnected && !timeoutReached {
				connectingChan <- true

				// set to false so we don't send the connectingChan again because no more listener
				initConnection = false
			}
		})

		select {
		case <-timeout.Done():
			glog.Warning("room: client is not connected after added, stopping client...")
			_ = client.stop()
			timeoutReached = true

		case <-connectingChan:
			return
		}
	}()

	client.OnJoined(func() {
		r.onClientJoined(client)
	})

	return client, nil
}

// Generate a unique client ID for this room
func (r *Room) CreateClientID() string {
	return GenerateID(21)
}

// Use this to get notified when a room is closed
func (r *Room) OnRoomClosed(callback func(id string)) {
	r.onRoomClosedCallbacks = append(r.onRoomClosedCallbacks, callback)
}

// Use this to get notified when a client is stopped and completly removed from the room
func (r *Room) OnClientLeft(callback func(client *Client)) {
	r.onClientLeftCallbacks = append(r.onClientLeftCallbacks, callback)
}

func (r *Room) onClientLeft(client *Client) {
	r.mu.RLock()
	callbacks := r.onClientLeftCallbacks
	exts := r.extensions
	r.mu.RUnlock()
	for _, callback := range callbacks {
		callback(client)
	}

	for _, ext := range exts {
		ext.OnClientRemoved(r, client)
	}

	// update the latest stats from client before they left
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stats[client.ID()] = client.stats.TrackStats
}

func (r *Room) onClientJoined(client *Client) {
	for _, callback := range r.onClientJoinedCallbacks {
		callback(client)
	}

	for _, ext := range r.extensions {
		ext.OnClientAdded(r, client)
	}
}

func (r *Room) OnClientJoined(callback func(client *Client)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.onClientJoinedCallbacks = append(r.onClientJoinedCallbacks, callback)
}

func (r *Room) SFU() *SFU {
	return r.sfu
}

// Get the room real time stats. This will return the current room stats.
// The client stats and it's tracks will be removed from the stats if the client or track is removed.
// But the aggregated stats will still be there and included in the room stats even if they're removed.
func (r *Room) Stats() RoomStats {
	var (
		bytesReceived    uint64
		bytesSent        uint64
		bitratesSent     uint64
		bitratesReceived uint64
	)

	clientStats := make(map[string]ClientTrackStats)

	r.mu.RLock()

	defer r.mu.RUnlock()

	for _, cstats := range r.stats {
		for _, stat := range cstats.bytesReceived {
			bytesReceived += stat
		}

		for _, stat := range cstats.senderBitrates {
			bitratesSent += uint64(stat)
		}

		for _, stat := range cstats.receiversBitrates {
			bitratesReceived += uint64(stat)
		}

		for _, stat := range cstats.bytesSent {
			bytesSent += stat
		}

	}

	roomStats := RoomStats{
		ActiveSessions: r.sfu.TotalActiveSessions(),
		ClientsCount:   0,
		BytesIngress:   bytesReceived,
		BytesEgress:    bytesSent,
		Timestamp:      time.Now(),
		ClientStats:    clientStats,
	}

	for id, c := range r.sfu.clients.GetClients() {
		roomStats.ClientStats[id] = c.Stats()

		roomStats.ClientsCount++

		for _, track := range roomStats.ClientStats[id].Receives {
			if track.Kind == webrtc.RTPCodecTypeAudio {
				roomStats.ReceivedTracks.Audio++
			} else {
				roomStats.ReceivedTracks.Video++
			}

			roomStats.BitrateReceived += uint64(track.CurrentBitrate)
		}

		for _, track := range roomStats.ClientStats[id].Sents {
			if track.Kind == webrtc.RTPCodecTypeAudio {
				roomStats.SentTracks.Audio++
			} else {
				roomStats.SentTracks.Video++
			}

			roomStats.BitrateSent += uint64(track.CurrentBitrate)
		}
	}

	return roomStats
}

func (r *Room) updateStats() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, client := range r.sfu.clients.GetClients() {
		r.stats[client.ID()] = client.stats.TrackStats
	}
}

func (r *Room) CreateDataChannel(label string, opts DataChannelOptions) error {
	return r.sfu.CreateDataChannel(label, opts)
}

// BitrateConfigs return the current bitrate configuration that used in bitrate controller
// Client should use this to configure the bitrate when publishing media tracks
// Inconsistent bitrate configuration between client and server will result missed bitrate calculation and
// could affecting packet loss and media quality
func (r *Room) BitrateConfigs() BitrateConfigs {
	return r.sfu.bitrateConfigs
}

// CodecPreferences return the current codec preferences that used in SFU
// Client should use this to configure the used codecs when publishing media tracks
// Inconsistent codec preferences between client and server can make the SFU cannot handle the codec properly
func (r *Room) CodecPreferences() []string {
	return r.sfu.codecs
}

func (r *Room) Context() context.Context {
	return r.context
}

func (r *Room) Meta() *Metadata {
	return r.meta
}

func (r *Room) Options() RoomOptions {
	return r.options
}

func (r *Room) loopRecordStats() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	ctx, cancel := context.WithCancel(r.context)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.updateStats()
		}
	}
}
