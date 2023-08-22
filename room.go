package sfu

import (
	"context"
	"sync"
	"time"
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
}

func DefaultOptions() Options {
	return Options{
		WebRTCPort:               50005,
		ConnectRemoteRoomTimeout: 30 * time.Second,
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
	Context                 context.Context
	cancelContext           context.CancelFunc
	ID                      string `json:"id"`
	RenegotiationChan       map[string]chan bool
	Name                    string `json:"name"`
	mutex                   *sync.Mutex
	sfu                     *SFU
	State                   string
	Type                    string
	extensions              []IExtension
	OnEvent                 func(event Event)
}

func newRoom(ctx context.Context, id, name string, sfu *SFU, roomType string) *Room {
	localContext, cancel := context.WithCancel(ctx)

	room := &Room{
		ID:            id,
		Context:       localContext,
		cancelContext: cancel,
		sfu:           sfu,
		State:         StateRoomOpen,
		Name:          name,
		mutex:         &sync.Mutex{},
		extensions:    make([]IExtension, 0),
		Type:          roomType,
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

	if len(r.sfu.GetClients()) > 0 {
		return ErrRoomIsNotEmpty
	}

	r.cancelContext()

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
	for _, client := range r.sfu.GetClients() {
		client.GetPeerConnection().Close()
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

	_, ok := r.sfu.clients[id]
	if ok {
		return nil, ErrClientExists
	}

	client := r.sfu.NewClient(id, opts)

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

func (r *Room) GetStats() RoomStats {
	return generateCurrentStats(r)
}
