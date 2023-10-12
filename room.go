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
	WebRTCPort               int
	ConnectRemoteRoomTimeout time.Duration
	EnableBridging           bool
	IceServers               []webrtc.ICEServer
}

func DefaultOptions() Options {
	return Options{
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
	RenegotiationChan       map[string]chan bool
	name                    string
	mutex                   *sync.Mutex
	sfu                     *SFU
	state                   string
	stats                   map[string]*ClientStats
	kind                    string
	extensions              []IExtension
	OnEvent                 func(event Event)
	options                 RoomOptions
}

type RoomOptions struct {
	Bitrates BitratesConfig
	Codecs   []string
	// Configures the timeout for client to join the room after register
	// The client will automatically kicked out from the room if not joined within the time after registered
	ClientTimeout time.Duration
	// Configures the interval between sending PLIs to clients that will generate keyframe
	// This will used for how often the video quality can be switched when the bandwitdh is changed
	PLIInterval time.Duration
}

func DefaultRoomOptions() RoomOptions {
	return RoomOptions{
		Bitrates:      DefaultBitrates(),
		Codecs:        []string{webrtc.MimeTypeVP9, webrtc.MimeTypeOpus},
		ClientTimeout: 10 * time.Minute,
		PLIInterval:   3 * time.Second,
	}
}

func newRoom(id, name string, sfu *SFU, kind string, opts RoomOptions) *Room {
	localContext, cancel := context.WithCancel(sfu.context)

	room := &Room{
		id:         id,
		context:    localContext,
		cancel:     cancel,
		sfu:        sfu,
		stats:      make(map[string]*ClientStats),
		state:      StateRoomOpen,
		name:       name,
		mutex:      &sync.Mutex{},
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
	var client *Client

	var err error

	if client, err = r.sfu.GetClient(id); err != nil {
		return err
	}

	return client.Stop()
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
				_ = client.Stop()
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

//nolint:copylocks // Statistic won't use the mutex
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
		if _, ok := clientStats[id]; !ok {
			clientStats[id] = &ClientTrackStats{
				ID:                       cstats.Client.id,
				Name:                     cstats.Client.name,
				ConsumerBandwidth:        cstats.Client.egressBandwidth.Load(),
				PublisherBandwidth:       cstats.Client.ingressBandwidth.Load(),
				Sents:                    make([]TrackSentStats, 0),
				Receives:                 make([]TrackReceiveStats, 0),
				CurrentPublishLimitation: cstats.Client.ingressQualityLimitationReason.Load().(string),
				CurrentConsumerBitrate:   cstats.Client.bitrateController.totalSentBitrates(),
			}
		}

		cstats.receiverMu.Lock()
		for _, stat := range cstats.Receiver {
			bytesReceived += stat.Stats.InboundRTPStreamStats.BytesReceived
			packetReceivedLost += stat.Stats.InboundRTPStreamStats.PacketsLost
			packetReceived += stat.Stats.InboundRTPStreamStats.PacketsReceived

			receivedStats := TrackReceiveStats{
				ID:             stat.Track.ID(),
				Kind:           stat.Track.Kind().String(),
				Codec:          stat.Track.Codec().MimeType,
				PacketsLost:    stat.Stats.InboundRTPStreamStats.PacketsLost,
				PacketReceived: stat.Stats.InboundRTPStreamStats.PacketsReceived,
			}

			clientStats[id].Receives = append(clientStats[id].Receives, receivedStats)
		}

		cstats.receiverMu.Unlock()

		cstats.senderMu.Lock()
		for _, stat := range cstats.Sender {
			bytesSent += stat.Stats.OutboundRTPStreamStats.BytesSent
			packetSentLost += stat.Stats.RemoteInboundRTPStreamStats.PacketsLost
			packetSent += stat.Stats.OutboundRTPStreamStats.PacketsSent

			claim := cstats.Client.bitrateController.GetClaim(stat.Track.ID())
			source := "media"

			if claim.track.IsScreen() {
				source = "screen"
			}

			sentStats := TrackSentStats{
				ID:             stat.Track.ID(),
				Kind:           stat.Track.Kind().String(),
				PacketsLost:    stat.Stats.RemoteInboundRTPStreamStats.PacketsLost,
				PacketSent:     stat.Stats.OutboundRTPStreamStats.PacketsSent,
				ByteSent:       stat.Stats.OutboundRTPStreamStats.BytesSent,
				CurrentBitrate: uint64(claim.track.getCurrentBitrate()),
				Source:         source,
				ClaimedBitrate: uint64(claim.bitrate),
				Quality:        claim.quality,
			}

			clientStats[id].Sents = append(clientStats[id].Sents, sentStats)
		}

		cstats.senderMu.Unlock()
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
