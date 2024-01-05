package sfu

import (
	"context"
	"errors"
	"sync"

	"github.com/pion/webrtc/v3"
)

var (
	ErrRoomNotFound             = errors.New("manager: room not found")
	ErrRoomAlreadyExists        = errors.New("manager: room already exists")
	ErrRemoteRoomConnectTimeout = errors.New("manager: timeout connecting to remote room")

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
	name       string
	mutex      sync.RWMutex
	options    Options
	extension  []IManagerExtension
}

func NewManager(ctx context.Context, name string, options Options) *Manager {
	var udpMux *UDPMux
	localCtx, cancel := context.WithCancel(ctx)

	if options.EnableMux {
		udpMux = NewUDPMux(ctx, options.WebRTCPort)
	}

	m := &Manager{
		rooms:      make(map[string]*Room),
		context:    localCtx,
		cancel:     cancel,
		iceServers: options.IceServers,
		udpMux:     udpMux,
		name:       name,
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
	return GenerateID()
}

func (m *Manager) Name() string {
	return m.name
}

func (m *Manager) NewRoom(id, name, roomType string, opts RoomOptions) (*Room, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, ok := m.rooms[id]; ok {
		return nil, ErrRoomAlreadyExists
	}

	err := m.onBeforeNewRoom(id, name, roomType)
	if err != nil {
		return nil, err
	}

	sfuOpts := sfuOptions{
		Bitrates:                 opts.Bitrates,
		IceServers:               m.iceServers,
		Mux:                      m.udpMux,
		PortStart:                m.options.PortStart,
		PortEnd:                  m.options.PortEnd,
		Codecs:                   opts.Codecs,
		PLIInterval:              opts.PLIInterval,
		QualityPreset:            opts.QualityPreset,
		EnableBandwidthEstimator: m.options.EnableBandwidthEstimator,
		PublicIP:                 m.options.PublicIP,
		NAT1To1IPsCandidateType:  m.options.NAT1To1IPsCandidateType,
	}

	newSFU := New(m.context, sfuOpts)

	room := newRoom(id, name, newSFU, roomType, opts)

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

	m.rooms[room.id] = room

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

// CloseRoom will stop all clients in the room and close it.
// This is a shortcut to find a room with id and close it.
func (m *Manager) CloseRoom(id string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	var room *Room

	var ok bool

	if room, ok = m.rooms[id]; !ok {
		return ErrRoomNotFound
	}

	return room.Close()
}

// Close will close all room and canceling the context
func (m *Manager) Close() {
	defer m.cancel()

	for _, room := range m.rooms {
		room.Close()
	}
}

func (m *Manager) Context() context.Context {
	return m.context
}
