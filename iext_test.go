package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

type testManagerExtension struct {
	onGetRoom       bool
	onBeforeNewRoom bool
	onNewRoom       bool
	onRoomClosed    bool
}

type testExtension struct {
	onBeforeClientAdded bool
	onClientAdded       bool
	onClientRemoved     bool
}

func NewTestExtension() *testExtension {
	return &testExtension{}
}

func (t *testExtension) OnBeforeClientAdded(room *Room, id string) error {
	t.onBeforeClientAdded = true
	return nil
}

func (t *testExtension) OnClientAdded(room *Room, client *Client) {
	t.onClientAdded = true
}

func (t *testExtension) OnClientRemoved(room *Room, client *Client) {
	t.onClientRemoved = true
}

func NewTestManagerExtension() *testManagerExtension {
	return &testManagerExtension{}
}

func (t *testManagerExtension) OnGetRoom(manager *Manager, roomID string) (*Room, error) {
	t.onGetRoom = true
	return nil, nil
}

func (t *testManagerExtension) OnBeforeNewRoom(id, name, roomType string) error {
	t.onBeforeNewRoom = true
	return nil
}

func (t *testManagerExtension) OnNewRoom(manager *Manager, room *Room) {
	t.onNewRoom = true
}

func (t *testManagerExtension) OnRoomClosed(manager *Manager, room *Room) {
	t.onRoomClosed = true
}

func TestManagerExtension(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := DefaultOptions()
	options.IceServers = DefaultTestIceServers()
	m := NewManager(ctx, "test", options)
	managerExt := NewTestManagerExtension()
	m.AddExtension(managerExt)
	roomID := m.CreateRoomID()

	room, err := m.NewRoom(roomID, "test", "p2p")
	require.NotNil(t, room, "room is nil")
	require.NoError(t, err, "error creating room")

	room.Close()

	_, err = m.GetRoom("wrong-room-id")

	require.NoError(t, err, "error getting room")

	require.True(t, managerExt.onGetRoom, "OnGetRoom is not called")

	require.True(t, managerExt.onBeforeNewRoom, "OnBeforeNewRoom is not called")

	require.True(t, managerExt.onNewRoom, "OnNewRoom is not called")

	require.True(t, managerExt.onRoomClosed, "OnRoomClosed is not called")
}

func TestExtension(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewManager(ctx, "test", Options{
		WebRTCPort:               50010,
		ConnectRemoteRoomTimeout: 30 * time.Second,
		IceServers:               DefaultTestIceServers(),
	})

	// create new room
	testRoom, err := m.NewRoom("test", "test", RoomTypeLocal)
	ext := NewTestExtension()
	testRoom.AddExtension(ext)

	require.NoError(t, err, "error creating room: %v", err)

	leftChan := make(chan bool)
	joinChan := make(chan bool)
	peerCount := 0

	tracks, _ := GetStaticTracks(ctx, "test", true)
	mediaEngine := GetMediaEngine()

	testRoom.OnClientLeft(func(client *Client) {
		leftChan <- true
	})

	testRoom.OnClientJoined(func(client *Client) {
		joinChan <- true
	})

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	client1, _ := testRoom.AddClient(testRoom.CreateClientID(testRoom.GetSFU().Counter), DefaultClientOptions())

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	peer1, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: DefaultTestIceServers(),
	})

	client1.OnIceCandidate = func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		err = peer1.AddICECandidate(candidate.ToJSON())
		require.NoErrorf(t, err, "error adding ice candidate: %v", err)
	}

	require.NoErrorf(t, err, "error creating peer connection: %v", err)
	SetPeerConnectionTracks(ctx, peer1, tracks)
	offer, err := peer1.CreateOffer(nil)
	require.NoErrorf(t, err, "error creating offer: %v", err)
	err = peer1.SetLocalDescription(offer)
	require.NoErrorf(t, err, "error setting local description: %v", err)
	answer, err := client1.Negotiate(offer)
	require.NoErrorf(t, err, "error negotiating offer: %v", err)
	err = peer1.SetRemoteDescription(*answer)
	require.NoErrorf(t, err, "error setting remote description: %v", err)
	peer1.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		err = client1.PeerConnection().AddICECandidate(candidate.ToJSON())
		require.NoErrorf(t, err, "error adding ice candidate: %v", err)

	})

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	for {
		select {
		case <-timeout.Done():
			t.Fatal("timeout waiting for client left event")
		case <-leftChan:
			glog.Info("client left")
			peerCount--
		case <-joinChan:
			glog.Info("client joined")
			peerCount++
			// stop client in go routine so we can receive left event
			go func() {
				_ = testRoom.StopClient(client1.ID())
			}()

		}

		glog.Info("peer count", peerCount)
		if peerCount == 0 {
			break
		}
	}

	_ = testRoom.Close()

	require.True(t, ext.onBeforeClientAdded, "OnBeforeClientAdded is not called")

	require.True(t, ext.onClientAdded, "OnClientAdded is not called")

	require.True(t, ext.onClientRemoved, "OnClientRemoved is not called")
}
