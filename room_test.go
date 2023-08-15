package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/testhelper"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestRoomCreateAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test", Options{})

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal)
	require.NoErrorf(t, err, "error creating new room: %v", err)

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	client1, err := testRoom.AddClient(testRoom.CreateClientID(testRoom.GetSFU().Counter), DefaultClientOptions())
	require.NoErrorf(t, err, "error adding client to room: %v", err)

	// stop client
	err = testRoom.StopClient(client1.ID)
	require.NoErrorf(t, err, "error stopping client: %v", err)

	client2, err := testRoom.AddClient(testRoom.CreateClientID(testRoom.GetSFU().Counter), DefaultClientOptions())
	require.NoErrorf(t, err, "error adding client to room: %v", err)

	// stop all clients should error on not empty room
	err = testRoom.Close()
	require.EqualError(t, err, ErrRoomIsNotEmpty.Error(), "expecting error room is not empty: %v", err)

	// stop other client
	err = testRoom.StopClient(client2.ID)
	require.NoErrorf(t, err, "error stopping client: %v", err)

	err = testRoom.Close()
	require.NoErrorf(t, err, "error closing room: %v", err)
}

func TestRoomJoinLeftEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test-join-left", Options{WebRTCPort: 40000})

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	clients := make(map[string]*Client)

	// create new room
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal)
	require.NoError(t, err, "error creating room: %v", err)
	leftChan := make(chan bool)
	joinChan := make(chan bool)
	peerCount := 0

	tracks, mediaEngine := testhelper.GetStaticTracks(ctx, "test")

	iceServers := []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	}

	testRoom.OnClientLeft(func(client *Client) {
		leftChan <- true
		glog.Info("client left", client.ID)
		delete(clients, client.ID)
	})

	testRoom.OnClientJoined(func(client *Client) {
		joinChan <- true
		glog.Info("client join", client.ID)
		clients[client.ID] = client
	})

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	client1, _ := testRoom.AddClient(testRoom.CreateClientID(testRoom.GetSFU().Counter), DefaultClientOptions())

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	peer1, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	client1.OnIceCandidate = func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		err = peer1.AddICECandidate(candidate.ToJSON())
		require.NoErrorf(t, err, "error adding ice candidate: %v", err)
	}

	require.NoErrorf(t, err, "error creating peer connection: %v", err)
	testhelper.SetPeerConnectionTracks(peer1, tracks)
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
		err = client1.GetPeerConnection().AddICECandidate(candidate.ToJSON())
		require.NoErrorf(t, err, "error adding ice candidate: %v", err)

	})

	timeout, cancelTimeout := context.WithTimeout(ctx, 20*time.Second)
	defer cancelTimeout()

	for {
		select {
		case <-timeout.Done():
			t.Fatal("timeout waiting for client left event")
		case <-leftChan:
			glog.Info("client left")
			peerCount--
		case <-joinChan:
			glog.Info("client join")
			peerCount++
			// stop client in go routine so we can receive left event
			go func() {
				_ = testRoom.StopClient(client1.ID)
			}()

		}

		glog.Info("peer count", peerCount)
		if peerCount == 0 {
			break
		}
	}

	_ = testRoom.Close()
}
