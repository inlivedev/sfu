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
	ConnectRemoteRoomTimeout time.Duration
	EnableBridging           bool
	EnableBandwidthEstimator bool
	IceServers               []webrtc.ICEServer
}

func DefaultOptions() Options {
	return Options{
		PortStart:                49152,
		PortEnd:                  65535,
		EnableMux:                false,
		WebRTCPort:               50005,
		ConnectRemoteRoomTimeout: 30 * time.Second,
		IceServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
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
	mutex                   *sync.Mutex
	meta                    *Metadata
	sfu                     *SFU
	state                   string
	stats                   map[string]*ClientStats
	kind                    string
	extensions              []IExtension
	OnEvent                 func(event Event)
	options                 RoomOptions
}

type RoomOptions struct {
	// Configures the bitrates configuration that will be used by the room
	// Make sure to use the same bitrate config when publishing video because this is used to manage the usage bandwidth in this room
	Bitrates BitratesConfig
	// Configures the codecs that will be used by the room
	Codecs []string
	// Configures the timeout for client to join the room after register
	// The client will automatically kicked out from the room if not joined within the time after registered
	ClientTimeout time.Duration
	// Configures the interval between sending PLIs to clients that will generate keyframe
	// More often means more bandwidth usage but more stability on video quality
	PLIInterval time.Duration
	// Configure the mapping of spatsial and temporal layers to quality level
	// Use this to use scalable video coding (SVC) to control the bitrate level of the video
	QualityPreset QualityPreset
}

func DefaultRoomOptions() RoomOptions {
	return RoomOptions{
		Bitrates:      DefaultBitrates(),
		QualityPreset: DefaultQualityPreset(),
		Codecs:        []string{"video/flexfec-03", webrtc.MimeTypeVP9, webrtc.MimeTypeH264, "audio/red", webrtc.MimeTypeOpus},
		ClientTimeout: 10 * time.Minute,
		PLIInterval:   0,
	}
}

func newRoom(id, name string, sfu *SFU, kind string, opts RoomOptions) *Room {
	localContext, cancel := context.WithCancel(sfu.context)

	room := &Room{
		id:         id,
		context:    localContext,
		cancel:     cancel,
		sfu:        sfu,
		token:      GenerateID(),
		stats:      make(map[string]*ClientStats),
		state:      StateRoomOpen,
		name:       name,
		mutex:      &sync.Mutex{},
		meta:       NewMetadata(),
		extensions: make([]IExtension, 0),
		kind:       kind,
		options:    opts,
	}

	sfu.OnClientRemoved(func(client *Client) {
		room.onClientLeft(client)
	})

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

// room can only closed once it's empty or it will return error
func (r *Room) Close() error {
	if r.state == StateRoomClosed {
		return ErrRoomIsClosed
	}

	if r.sfu.clients.Length() > 0 {
		return ErrRoomIsNotEmpty
	}

	r.cancel()

	r.sfu.Stop()

	for _, callback := range r.onRoomClosedCallbacks {
		callback(r.id)
	}

	r.state = StateRoomClosed

	return nil
}

// Stopping client is async, it will just stop the client and return immediately
// You should use OnClientLeft to get notified when the client is actually stopped
func (r *Room) StopClient(id string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	var client *Client

	var err error

	if client, err = r.sfu.GetClient(id); err != nil {
		return err
	}

	return client.stop()
}

func (r *Room) StopAllClients() {
	for _, client := range r.sfu.clients.GetClients() {
		client.PeerConnection().Close()
	}
}

func (r *Room) AddClient(id, name string, opts ClientOptions) (*Client, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

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
	go func() {
		timeout, cancel := context.WithTimeout(client.context, r.options.ClientTimeout)
		defer cancel()

		connectingChan := make(chan bool)

		client.OnConnectionStateChanged(func(state webrtc.PeerConnectionState) {
			if state == webrtc.PeerConnectionStateConnected {
				connectingChan <- true
			}
		})

		select {
		case <-timeout.Done():
			if timeout.Err() == context.DeadlineExceeded {
				glog.Warning("room: client is not connected after added, stopping client...")
				_ = r.StopClient(client.ID())
			}

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
	return GenerateID()
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
	for _, callback := range r.onClientLeftCallbacks {
		callback(client)
	}

	for _, ext := range r.extensions {
		ext.OnClientRemoved(r, client)
	}

	// update the latest stats from client before they left
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.stats[client.ID()] = client.Stats()
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
	r.onClientJoinedCallbacks = append(r.onClientJoinedCallbacks, callback)
}

func (r *Room) SFU() *SFU {
	return r.sfu
}

func (r *Room) Stats() RoomStats {
	var (
		bytesReceived      uint64
		bytesSent          uint64
		packetSentLost     int64
		packetSent         uint64
		packetReceivedLost int64
		packetReceived     uint64
	)

	// make sure the stats is up to date
	r.updateStats()

	clientStats := make(map[string]*ClientTrackStats)

	r.mutex.Lock()

	defer r.mutex.Unlock()

	for id, cstats := range r.stats {
		if cstats.Client != nil {
			stats := cstats.Client.TrackStats()
			if stats != nil {
				clientStats[id] = stats
				for _, stat := range stats.Receives {
					bytesReceived += uint64(stat.BytesReceived)
					packetReceivedLost += stat.PacketsLost
					packetReceived += stat.PacketsReceived
				}

				for _, stat := range stats.Sents {
					bytesSent += stat.BytesSent
					packetSentLost += stat.PacketsLost
					packetSent += stat.PacketSent
				}
			}
		}

	}

	return RoomStats{
		ActiveSessions:     r.sfu.TotalActiveSessions(),
		ClientsCount:       len(r.stats),
		PacketSentLost:     packetSentLost,
		PacketReceivedLost: packetReceivedLost,
		PacketReceived:     packetReceived,
		PacketSent:         packetSent,
		BytesReceived:      bytesReceived,
		ByteSent:           bytesSent,
		Timestamp:          time.Now(),
		ClientStats:        clientStats,
	}
}

func (r *Room) updateStats() {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	for _, client := range r.sfu.clients.GetClients() {
		r.stats[client.ID()] = client.Stats()
	}
}

func (r *Room) CreateDataChannel(label string, opts DataChannelOptions) error {
	return r.sfu.CreateDataChannel(label, opts)
}

func (r *Room) BitratesConfig() BitratesConfig {
	return r.sfu.bitratesConfig
}

func (r *Room) Context() context.Context {
	return r.context
}

func (r *Room) Meta() *Metadata {
	return r.meta
}
