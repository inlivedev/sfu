package sfu

import (
	"context"
	"sync"
	"time"

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
	ID                      string `json:"id"`
	RenegotiationChan       map[string]chan bool
	Name                    string `json:"name"`
	mutex                   *sync.Mutex
	sfu                     *SFU
	State                   string
	stats                   map[string]ClientStats
	Type                    string
	extensions              []IExtension
	OnEvent                 func(event Event)
}

func newRoom(ctx context.Context, id, name string, sfu *SFU, roomType string) *Room {
	localContext, cancel := context.WithCancel(ctx)

	room := &Room{
		ID:         id,
		context:    localContext,
		cancel:     cancel,
		sfu:        sfu,
		stats:      make(map[string]ClientStats),
		State:      StateRoomOpen,
		Name:       name,
		mutex:      &sync.Mutex{},
		extensions: make([]IExtension, 0),
		Type:       roomType,
	}

	sfu.OnClientRemoved(func(client *Client) {
		room.onClientLeft(client)
	})

	return room
}

func (r *Room) AddExtension(extension IExtension) {
	r.extensions = append(r.extensions, extension)
}

// room can only closed once it's empty or it will return error
func (r *Room) Close() error {
	if r.State == StateRoomClosed {
		return ErrRoomIsClosed
	}

	if r.sfu.clients.Length() > 0 {
		return ErrRoomIsNotEmpty
	}

	r.cancel()

	r.sfu.Stop()

	for _, callback := range r.onRoomClosedCallbacks {
		callback(r.ID)
	}

	r.State = StateRoomClosed

	return nil
}

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

func (r *Room) AddClient(id string, opts ClientOptions) (*Client, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.State == StateRoomClosed {
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

	client = r.sfu.NewClient(id, opts)

	client.OnJoined(func() {
		r.onClientJoined(client)
	})

	return client, nil
}

func (r *Room) CreateClientID(id int) string {
	if id == 0 {
		return GenerateID([]int{r.sfu.Counter})
	}

	return GenerateID([]int{r.sfu.Counter, id})
}

func (r *Room) OnRoomClosed(callback func(id string)) {
	r.onRoomClosedCallbacks = append(r.onRoomClosedCallbacks, callback)
}

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

	r.stats[client.ID()] = *client.Stats()
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

func (r *Room) GetSFU() *SFU {
	return r.sfu
}

func (r *Room) GetID() string {
	return r.ID
}

func (r *Room) GetName() string {
	return r.Name
}

//nolint:copylocks // Statistic won't use the mutex
func (r *Room) GetStats() RoomStats {
	var (
		bytesReceived uint64
		bytesSent     uint64
		packet_lost   int64
	)

	// make sure the stats is up to date
	r.updateStats()

	for _, cstats := range r.stats {
		for _, stat := range cstats.Receiver {
			bytesReceived += stat.InboundRTPStreamStats.BytesReceived
			packet_lost += stat.InboundRTPStreamStats.PacketsLost
		}

		for _, stat := range cstats.Sender {
			bytesSent += stat.OutboundRTPStreamStats.BytesSent
		}
	}

	return RoomStats{
		ActiveSessions: r.sfu.TotalActiveSessions(),
		Clients:        len(r.stats),
		PacketLost:     packet_lost,
		BytesReceived:  bytesReceived,
		ByteSent:       bytesSent,
		Timestamp:      time.Now(),
	}
}

func (r *Room) updateStats() {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	for _, client := range r.sfu.clients.GetClients() {
		r.stats[client.ID()] = *client.Stats()
	}
}

func (r *Room) CreateDataChannel(label string, opts DataChannelOptions) error {
	return r.sfu.CreateDataChannel(label, opts)
}

func (r *Room) GetBitratesConfig() BitratesConfig {
	return r.sfu.bitratesConfig
}
