package sfu

import (
	"context"
	"log"
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
	callbacksClientRemoved []func(id string)
	callbacksRoomClosed    []func(id string)
	Context                context.Context
	cancelContext          context.CancelFunc
	eventChan              chan Event
	ID                     string `json:"id"`
	RenegotiationChan      map[string]chan bool
	Name                   string `json:"name"`
	mutex                  *sync.Mutex
	sfu                    *SFU
	State                  string
	Type                   string
	extensions             []IExtension
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

	return room
}

func (r *Room) AddExtension(extension IExtension) {
	r.extensions = append(r.extensions, extension)
}

// room should not close manually, it will be close once no client is in the room automatically
// this is to prevent recursive close callback
// use StopAllClients() to close room manually that will triggered this callback once all clients are closed
func (r *Room) close() error {
	if r.State == StateRoomClosed {
		return ErrRoomIsClosed
	}

	if len(r.sfu.GetClients()) > 0 {
		return ErrRoomIsNotEmpty
	}

	r.cancelContext()

	r.sfu.Stop()

	for _, callback := range r.callbacksRoomClosed {
		callback(r.ID)
	}

	r.State = StateRoomClosed

	r.sendEvent(EventRoomClosed, nil)

	return nil
}

func (r *Room) StopClient(id string) error {
	var client *Client

	var err error

	if client, err = r.sfu.GetClient(id); err != nil {
		return err
	}

	return client.GetPeerConnection().Close()
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

	_, ok := r.sfu.clients[id]
	if ok {
		return nil, ErrClientExists
	}

	client := r.sfu.NewClient(id, opts)

	client.OnStopped(func() {
		log.Println("client stopped ", client.ID)
		r.RemoveClient(client.ID)
	})

	for _, ext := range r.extensions {
		ext.OnClientAdded(client)
	}

	return client, nil
}

func (r *Room) CreateClientID(id int) string {
	if id == 0 {
		return GenerateID([]int{r.sfu.Counter})
	}

	return GenerateID([]int{r.sfu.Counter, id})
}

func (r *Room) RemoveClient(id string) {
	r.onClientRemoved(id)
}

func (r *Room) OnRoomClosed(callback func(id string)) {
	r.callbacksRoomClosed = append(r.callbacksRoomClosed, callback)
}

func (r *Room) OnClientRemoved(callback func(id string)) {
	r.callbacksClientRemoved = append(r.callbacksClientRemoved, callback)
}

func (r *Room) onClientRemoved(clientid string) {
	for _, callback := range r.callbacksClientRemoved {
		callback(clientid)
	}

	r.sendEvent(EventRoomClientLeft, map[string]interface{}{
		"clientid": clientid,
	})

	if len(r.sfu.GetClients()) == 0 {
		r.close()
	}
}

func (r *Room) sendEvent(eventType string, data map[string]interface{}) {
	r.eventChan <- Event{
		Type: eventType,
		Time: time.Now(),
		Data: data,
	}
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

func (r *Room) GetEventChan() chan Event {
	return r.eventChan
}
