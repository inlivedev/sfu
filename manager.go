package sfu

import (
	"context"
	"errors"
	"sync"

	"github.com/pion/webrtc/v3"
)

var (
	ErrRoomNotFound             = errors.New("room not found")
	ErrRemoteRoomConnectTimeout = errors.New("timeout connecting to remote room")

	RoomTypeLocal  = "local"
	RoomTypeRemote = "remote"
)

// Manager is a struct that manages all the rooms
type Manager struct {
	rooms      map[string]*Room
	context    context.Context
	cancel     context.CancelFunc
	iceServers []webrtc.ICEServer
	udpMux     *UDPMux
	Name       string
	mutex      sync.RWMutex
	options    Options
	extension  []IManagerExtension
}

func NewManager(ctx context.Context, name string, options Options) *Manager {
	localCtx, cancel := context.WithCancel(ctx)

	udpMux := NewUDPMux(ctx, options.WebRTCPort)

	m := &Manager{
		rooms:      make(map[string]*Room),
		context:    localCtx,
		cancel:     cancel,
		iceServers: options.IceServers,
		udpMux:     udpMux,
		Name:       name,
		mutex:      sync.RWMutex{},
		options:    options,
		extension:  make([]IManagerExtension, 0),
	}

	return m
}

func (m *Manager) AddExtension(extension IManagerExtension) {
	m.extension = append(m.extension, extension)
}

func (m *Manager) CreateRoomID() string {
	return GenerateID([]int{len(m.rooms)})
}

func (m *Manager) NewRoom(id, name, roomType string, bitrates BitratesConfig) (*Room, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	err := m.onBeforeNewRoom(id, name, roomType)
	if err != nil {
		return nil, err
	}

	sfuOpts := sfuOptions{
		Bitrates:   bitrates,
		IceServers: m.iceServers,
		Mux:        m.udpMux,
	}

	newSFU := New(m.context, sfuOpts)

	room := newRoom(id, name, newSFU, roomType)

	for _, ext := range m.extension {
		ext.OnNewRoom(m, room)
	}

	// TODO: what manager should do when a room is closed?
	// is there any neccesary resource to be released?
	room.OnRoomClosed(func(id string) {
		for _, ext := range m.extension {
			ext.OnRoomClosed(m, room)
		}
	})

	room.OnClientLeft(func(client *Client) {
		// TODO: should check if the room is empty and close it if it is

	})

	m.rooms[room.ID] = room

	return room, nil
}

func (m *Manager) onBeforeNewRoom(id, name, roomType string) error {
	for _, ext := range m.extension {
		err := ext.OnBeforeNewRoom(id, name, roomType)
		if err != nil {
			return err
		}

	}

	return nil
}

func (m *Manager) RoomsCount() int {
	return len(m.rooms)
}

func (m *Manager) GetRoom(id string) (*Room, error) {
	var (
		room *Room
		err  error
	)

	room, err = m.getRoom(id)
	if err == ErrRoomNotFound {
		for _, ext := range m.extension {
			room, err = ext.OnGetRoom(m, id)
			if err == nil && room != nil {
				return room, nil
			}
		}

		return room, err
	}

	return room, nil
}

func (m *Manager) getRoom(id string) (*Room, error) {
	var (
		room *Room
		ok   bool
	)

	if room, ok = m.rooms[id]; !ok {
		return nil, ErrRoomNotFound
	}

	return room, nil
}

func (m *Manager) EndRoom(id string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	var room *Room

	var ok bool

	if room, ok = m.rooms[id]; !ok {
		return ErrRoomNotFound
	}

	room.StopAllClients()

	return nil
}

func (m *Manager) Stop() {
	defer m.cancel()

	for _, room := range m.rooms {
		room.StopAllClients()
	}
}
