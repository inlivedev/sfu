package sfu

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/testhelper"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
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

	_, client1, _, _ := createPeerPair(t, ctx, testRoom, "peer1", false)

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

func TestRoomStats(t *testing.T) {
	var (
		totalClientIngressBytes uint64
		totalClientEgressBytes  uint64
	)

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
	joinChan := make(chan bool)
	peerCount := 0

	testRoom.OnClientJoined(func(client *Client) {
		joinChan <- true
		glog.Info("client join", client.ID)
		clients[client.ID] = client
	})

	pc1, _, statsGetter1, done1 := createPeerPair(t, ctx, testRoom, "peer1", false)
	pc2, _, statsGetter2, done2 := createPeerPair(t, ctx, testRoom, "peer2", false)

	timeout, cancelTimeout := context.WithTimeout(ctx, 80*time.Second)
	defer cancelTimeout()

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case <-done1:
			peerCount++
			glog.Info("test: pc1 done")
		case <-done2:
			peerCount++
			glog.Info("test: pc2 done")
		default:
			// this will trying to break out after all audio video packets are received

			if peerCount == 2 {
				time.Sleep(2 * time.Second)
				pc1ReceiverStats := GetReceiverStats(pc1, statsGetter1)
				pc1SenderStats := GetSenderStats(pc1, statsGetter1)
				pc2ReceiverStats := GetReceiverStats(pc2, statsGetter2)
				pc2SenderStats := GetSenderStats(pc2, statsGetter2)

				totalClientIngressBytes = 0
				totalClientEgressBytes = 0

				for _, stat := range pc1ReceiverStats {
					totalClientIngressBytes += stat.InboundRTPStreamStats.BytesReceived
				}

				for _, stat := range pc2ReceiverStats {
					totalClientIngressBytes += stat.InboundRTPStreamStats.BytesReceived
				}

				for _, stat := range pc1SenderStats {
					totalClientEgressBytes += stat.OutboundRTPStreamStats.BytesSent
				}

				for _, stat := range pc2SenderStats {
					totalClientEgressBytes += stat.OutboundRTPStreamStats.BytesSent
				}

				roomStats := testRoom.GetStats()

				if totalClientIngressBytes == roomStats.ByteSent &&
					totalClientEgressBytes == roomStats.BytesReceived {
					break Loop
				}

				glog.Info("total client ingress bytes: ", totalClientIngressBytes)
				glog.Info("total client egress bytes: ", totalClientEgressBytes)
				glog.Info("total room bytes sent: ", roomStats.ByteSent)
				glog.Info("total room bytes receive: ", roomStats.BytesReceived)
				glog.Info("total room packet lost: ", roomStats.PacketLost)
			}
		}
	}

	glog.Info("total client ingress bytes: ", totalClientIngressBytes)
	glog.Info("total client egress bytes: ", totalClientEgressBytes)

	glog.Info("get room stats")
	roomStats := testRoom.GetStats()

	require.NotEqual(t, uint64(0), totalClientEgressBytes)
	require.NotEqual(t, uint64(0), totalClientIngressBytes)

	glog.Info("total room bytes sent: ", roomStats.ByteSent)
	glog.Info("total room bytes receive: ", roomStats.BytesReceived)
	glog.Info("total room packet lost: ", roomStats.PacketLost)

	diffPercentClientIgressRoomBytesSent := (float64(totalClientIngressBytes) - float64(roomStats.ByteSent)) / float64(totalClientIngressBytes) * 100
	require.LessOrEqual(t, diffPercentClientIgressRoomBytesSent, 10.0, "expecting less than 10 percent difference client igress and room byte sent")

	diffPercentClientEgressRoomBytesReceived := (float64(totalClientEgressBytes) - float64(roomStats.BytesReceived)) / float64(totalClientEgressBytes) * 100
	require.LessOrEqual(t, diffPercentClientEgressRoomBytesReceived, 10.0, "expecting less than 10 percent difference client egress and room byte received")

	glog.Info(totalClientIngressBytes, roomStats.ByteSent)
}

func createPeerPair(t *testing.T, ctx context.Context, testRoom *Room, peerName string, loop bool) (*webrtc.PeerConnection, *Client, stats.Getter, chan bool) {
	t.Helper()

	var client *Client

	iceServers := []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	}
	tracks, mediaEngine, done := testhelper.GetStaticTracks(ctx, peerName, loop)

	i := &interceptor.Registry{}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	var statsGetter stats.Getter

	statsInterceptorFactory.OnNewPeerConnection(func(_ string, g stats.Getter) {
		statsGetter = g
	})

	i.Add(statsInterceptorFactory)

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, i); err != nil {
		panic(err)
	}

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i))

	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	require.NoErrorf(t, err, "error creating peer connection: %v", err)

	testhelper.SetPeerConnectionTracks(ctx, pc, tracks)

	allDone := make(chan bool)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		glog.Info("test: got track ", peerName, track.Kind().String())
		go func() {
			ctxx, cancel := context.WithCancel(ctx)
			defer cancel()

			rtpBuff := make([]byte, 1500)
			for {
				select {
				case <-ctxx.Done():
					return
				default:
					_, _, err = track.Read(rtpBuff)
					if err == io.EOF {
						return
					}
				}

			}
		}()
	})

	go func() {
		ctxx, cancel := context.WithCancel(ctx)
		defer cancel()

		for {
			select {
			case <-ctxx.Done():
				return

			case <-done:
				allDone <- true
			}
		}
	}()

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	client, _ = testRoom.AddClient(testRoom.CreateClientID(testRoom.GetSFU().Counter), DefaultClientOptions())

	client.OnAllowedRemoteRenegotiation = func() {
		glog.Info("allowed remote renegotiation")
		negotiate(t, pc, client)
	}

	client.OnIceCandidate = func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	}

	client.OnRenegotiation = func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.State == ClientStateEnded {
			glog.Info("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		glog.Info("test: got renegotiation ", peerName)
		err = pc.SetRemoteDescription(offer)
		require.NoErrorf(t, err, "error setting remote description: %v", err)
		answer, err = pc.CreateAnswer(nil)
		require.NoErrorf(t, err, "error creating answer: %v", err)
		err = pc.SetLocalDescription(answer)
		require.NoErrorf(t, err, "error setting local description: %v", err)
		return *pc.LocalDescription(), nil
	}

	require.NoErrorf(t, err, "error creating peer connection: %v", err)

	negotiate(t, pc, client)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		err = client.GetPeerConnection().AddICECandidate(candidate.ToJSON())
		require.NoErrorf(t, err, "error adding ice candidate: %v", err)

	})

	return pc, client, statsGetter, allDone
}

func negotiate(t *testing.T, pc *webrtc.PeerConnection, client *Client) {
	t.Helper()
	if pc.SignalingState() != webrtc.SignalingStateStable {
		glog.Info("test: signaling state is not stable, skip renegotiation")
		return
	}

	offer, err := pc.CreateOffer(nil)
	require.NoErrorf(t, err, "error creating offer: %v", err)
	err = pc.SetLocalDescription(offer)
	require.NoErrorf(t, err, "error setting local description: %v", err)
	answer, err := client.Negotiate(offer)
	require.NoErrorf(t, err, "error negotiating offer: %v", err)
	err = pc.SetRemoteDescription(*answer)
	require.NoErrorf(t, err, "error setting remote description: %v", err)
}
