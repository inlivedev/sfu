package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestRoomCreateAndClose(t *testing.T) {
	t.Parallel()
	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoErrorf(t, err, "error creating new room: %v", err)

	clientsLeft := 0
	testRoom.OnClientLeft(func(client *Client) {
		clientsLeft++
	})

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	id := testRoom.CreateClientID()
	client1, err := testRoom.AddClient(id, id, DefaultClientOptions())
	require.NoErrorf(t, err, "error adding client to room: %v", err)

	// stop client
	err = testRoom.StopClient(client1.ID())
	require.NoErrorf(t, err, "error stopping client: %v", err)

	id = testRoom.CreateClientID()
	client2, err := testRoom.AddClient(id, id, DefaultClientOptions())
	require.NoErrorf(t, err, "error adding client to room: %v", err)

	// stop all clients should error on unempty room
	err = testRoom.Close()
	require.EqualError(t, err, ErrRoomIsNotEmpty.Error(), "expecting error room is not empty: %v", err)

	// stop other client
	err = testRoom.StopClient(client2.ID())
	require.NoErrorf(t, err, "error stopping client: %v", err)

Loop:
	for {
		timeout, cancelTimeout := context.WithTimeout(testRoom.sfu.context, 10*time.Second)
		defer cancelTimeout()
		select {
		case <-timeout.Done():
			t.Fatal("timeout waiting for client left event")
			break Loop
		default:
			if clientsLeft == 2 {
				break Loop
			}

			time.Sleep(100 * time.Millisecond)
		}
	}

	require.Equal(t, 2, clientsLeft)

	err = testRoom.Close()
	require.NoErrorf(t, err, "error closing room: %v", err)
}

func TestRoomJoinLeftEvent(t *testing.T) {
	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	clients := make(map[string]*Client)

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	leftChan := make(chan bool)
	joinChan := make(chan string)
	peerCount := 0

	testRoom.OnClientLeft(func(client *Client) {
		leftChan <- true
		t.Log("client left", client.ID())
		delete(clients, client.ID())
	})

	testRoom.OnClientJoined(func(client *Client) {
		joinChan <- client.ID()
		t.Log("client join", client.ID())
		clients[client.ID()] = client
	})

	ctx := testRoom.sfu.context

	_, client1, _, _ := CreatePeerPair(ctx, testRoom, DefaultTestIceServers(), "peer1", false, false)
	_, client2, _, _ := CreatePeerPair(ctx, testRoom, DefaultTestIceServers(), "peer1", false, false)
	_, client3, _, _ := CreatePeerPair(ctx, testRoom, DefaultTestIceServers(), "peer1", false, false)

	timeout, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	defer cancelTimeout()

	peerLeft := 0

	for {
		select {
		case <-timeout.Done():
			t.Fatal("timeout waiting for client left event")
		case <-leftChan:
			t.Log("client left")
			peerLeft++
		case id := <-joinChan:
			t.Log("client join")
			peerCount++
			// stop client in go routine so we can receive left event
			go func() {
				switch id {
				case client1.ID():
					_ = testRoom.StopClient(client1.ID())
				case client2.ID():
					err := testRoom.StopClient(client2.ID())
					require.NoError(t, err, "error stopping client: %v", err)
				case client3.ID():
					client3.PeerConnection().Close()
					require.NoError(t, err, "error stopping client: %v", err)
				}
			}()
		}

		t.Log("peer count", peerCount)
		if peerLeft == 3 {
			break
		}
	}

	require.Equal(t, 0, len(testRoom.sfu.clients.clients))

	_ = testRoom.Close()
}

func TestRoomStats(t *testing.T) {
	t.Parallel()

	var (
		totalClientIngressBytes uint64
		totalClientEgressBytes  uint64
	)

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	clients := make(map[string]*Client)

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	peerCount := 0

	ctx := testRoom.sfu.context

	testRoom.OnClientJoined(func(client *Client) {
		clients[client.ID()] = client
	})

	pc1, client1, statsGetter1, done1 := CreatePeerPair(ctx, testRoom, DefaultTestIceServers(), "peer1", false, false)
	client1.SubscribeAllTracks()

	client1.OnTracksAdded(func(addedTracks []ITrack) {
		setTracks := make(map[string]TrackType, 0)
		for _, track := range addedTracks {
			setTracks[track.ID()] = TrackTypeMedia
		}
		client1.SetTracksSourceType(setTracks)
	})

	pc2, client2, statsGetter2, done2 := CreatePeerPair(ctx, testRoom, DefaultTestIceServers(), "peer2", false, false)
	client2.SubscribeAllTracks()

	client2.OnTracksAdded(func(addedTracks []ITrack) {
		setTracks := make(map[string]TrackType, 0)
		for _, track := range addedTracks {
			setTracks[track.ID()] = TrackTypeMedia
		}
		client2.SetTracksSourceType(setTracks)
	})

	timeout, cancelTimeout := context.WithTimeout(ctx, 80*time.Second)
	defer cancelTimeout()

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case <-done1:
			peerCount++
			t.Log("test: pc1 done")
		case <-done2:
			peerCount++
			t.Log("test: pc2 done")
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

				roomStats := testRoom.Stats()

				diffPercentClientIgressRoomBytesSent := (float64(totalClientIngressBytes) - float64(roomStats.ByteSent)) / float64(totalClientIngressBytes) * 100
				diffPercentClientEgressRoomBytesReceived := (float64(totalClientEgressBytes) - float64(roomStats.BytesReceived)) / float64(totalClientEgressBytes) * 100

				if diffPercentClientIgressRoomBytesSent < 10.0 &&
					diffPercentClientEgressRoomBytesReceived < 10.0 {
					break Loop
				}

				t.Log("total client ingress bytes: ", totalClientIngressBytes)
				t.Log("total client egress bytes: ", totalClientEgressBytes)
				t.Log("total room bytes sent: ", roomStats.ByteSent)
				t.Log("total room bytes receive: ", roomStats.BytesReceived)
				t.Log("total room packet received lost: ", roomStats.PacketReceivedLost)
				t.Log("total room packet sent lost: ", roomStats.PacketSentLost)
			}
		}
	}

	t.Log("total client ingress bytes: ", totalClientIngressBytes)
	t.Log("total client egress bytes: ", totalClientEgressBytes)

	t.Log("get room stats")
	roomStats := testRoom.Stats()

	require.NotEqual(t, uint64(0), totalClientEgressBytes)
	require.NotEqual(t, uint64(0), totalClientIngressBytes)

	t.Log("total room bytes sent: ", roomStats.ByteSent)
	t.Log("total room bytes receive: ", roomStats.BytesReceived)
	t.Log("total room packet sent lost: ", roomStats.PacketSentLost)
	t.Log("total room packet received lost: ", roomStats.PacketReceivedLost)

	diffPercentClientIgressRoomBytesSent := (float64(totalClientIngressBytes) - float64(roomStats.ByteSent-uint64(roomStats.PacketSentLost*1500))) / float64(totalClientIngressBytes) * 100
	require.LessOrEqual(t, diffPercentClientIgressRoomBytesSent, 10.0, "expecting less than 10 percent difference client igress and room byte sent")

	diffPercentClientEgressRoomBytesReceived := (float64(totalClientEgressBytes) - float64(roomStats.BytesReceived)) / float64(totalClientEgressBytes) * 100
	require.LessOrEqual(t, diffPercentClientEgressRoomBytesReceived, 10.0, "expecting less than 10 percent difference client egress and room byte received")

	t.Log(totalClientIngressBytes, roomStats.ByteSent)
}

func TestRoomAddClientTimeout(t *testing.T) {
	t.Parallel()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	timeout := 5 * time.Second
	roomOpts.ClientTimeout = timeout
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoErrorf(t, err, "error creating new room: %v", err)

	ctx := testRoom.sfu.context

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	id := testRoom.CreateClientID()

	client, err := testRoom.AddClient(id, id, DefaultClientOptions())
	require.NoErrorf(t, err, "error adding client to room: %v", err)

	clientRemovedChan := make(chan *Client)

	testRoom.SFU().OnClientRemoved(func(c *Client) {
		clientRemovedChan <- c
	})

	ctxTimeout, cancelTimeout := context.WithTimeout(ctx, 20*time.Second)
	defer cancelTimeout()

	select {
	case <-ctxTimeout.Done():
		t.Fatal("timeout waiting for client removed event")
	case c := <-clientRemovedChan:
		require.Equal(t, c.ID(), client.ID())
	}
}
