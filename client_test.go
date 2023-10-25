package sfu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestTracksManualSubscribe(t *testing.T) {
	t.Parallel()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	peerCount := 5

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	ctx := testRoom.sfu.context

	tracksAddedChan := make(chan int)
	tracksAvailableChan := make(chan int)
	trackChan := make(chan bool)

	for i := 0; i < peerCount; i++ {
		pc, client, _, _ := CreatePeerPair(ctx, testRoom, DefaultTestIceServers(), fmt.Sprintf("peer-%d", i), true, false)
		client.OnTracksAvailable(func(availableTracks []ITrack) {
			tracksAvailableChan <- len(availableTracks)
			tracksReq := make([]SubscribeTrackRequest, 0)
			for _, track := range availableTracks {
				tracksReq = append(tracksReq, SubscribeTrackRequest{
					ClientID: track.Client().ID(),
					TrackID:  track.ID(),
				})
			}
			err := client.SubscribeTracks(tracksReq)
			require.NoError(t, err)
		})

		client.OnTracksAdded(func(addedTracks []ITrack) {
			tracksAddedChan <- len(addedTracks)

			setTracks := make(map[string]TrackType)

			for _, track := range addedTracks {
				setTracks[track.ID()] = TrackTypeMedia
			}
			client.SetTracksSourceType(setTracks)
		})

		pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			trackChan <- true
		})
	}

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	tracksAdded := 0
	tracksAvailable := 0
	trackReceived := 0
	expectedTracks := (peerCount * 2) * (peerCount - 1)

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case added := <-tracksAddedChan:
			tracksAdded += added
		case available := <-tracksAvailableChan:
			tracksAvailable += available
		case <-trackChan:
			trackReceived++
			if trackReceived == expectedTracks {
				break Loop
			}
		}

	}

	require.Equal(t, peerCount*2, tracksAdded)
	require.Equal(t, expectedTracks, trackReceived)
}

func TestAutoSubscribeTracks(t *testing.T) {
	t.Parallel()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	peerCount := 5

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	ctx := testRoom.sfu.context

	trackChan := make(chan bool)

	for i := 0; i < peerCount; i++ {
		pc, client, _, _ := CreatePeerPair(ctx, testRoom, DefaultTestIceServers(), fmt.Sprintf("peer-%d", i), true, false)
		client.SubscribeAllTracks()

		client.OnTracksAdded(func(addedTracks []ITrack) {
			setTracks := make(map[string]TrackType, 0)
			for _, track := range addedTracks {
				setTracks[track.ID()] = TrackTypeMedia
			}
			client.SetTracksSourceType(setTracks)
		})

		pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			trackChan <- true
		})
	}

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	trackReceived := 0
	expectedTracks := (peerCount * 2) * (peerCount - 1)

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop

		case <-trackChan:
			trackReceived++
			if trackReceived == expectedTracks {
				break Loop
			}
		}

	}

	require.Equal(t, expectedTracks, trackReceived)
}

// TODO: this is can't be work without a new SimulcastLocalTrack that can add header extension to the packet

func TestSimulcastTrack(t *testing.T) {
	t.Parallel()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	ctx := testRoom.sfu.context

	client1, pc1 := addSimulcastPair(t, ctx, testRoom, "peer1")
	client2, pc2 := addSimulcastPair(t, ctx, testRoom, "peer2")

	defer func() {
		_ = testRoom.StopClient(client1.id)
		_ = testRoom.StopClient(client2.id)
	}()

	trackChan := make(chan *webrtc.TrackRemote)

	pc1.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		trackChan <- track
	})

	pc2.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		trackChan <- track
	})

	// wait for track added
	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	trackCount := 0
Loop:
	for {
		select {
		case <-timeout.Done():
			t.Fatal("timeout waiting for track added")
		case <-trackChan:
			trackCount++
			glog.Info("track added ", trackCount)
			if trackCount == 2 {
				break Loop
			}
		}
	}
}

func addSimulcastPair(t *testing.T, ctx context.Context, room *Room, peerName string) (*Client, *webrtc.PeerConnection) {
	pc, client, _, _ := CreatePeerPair(ctx, room, DefaultTestIceServers(), peerName, true, true)
	client.OnTracksAvailable(func(availableTracks []ITrack) {

		tracksReq := make([]SubscribeTrackRequest, 0)
		for _, track := range availableTracks {
			tracksReq = append(tracksReq, SubscribeTrackRequest{
				ClientID: track.Client().ID(),
				TrackID:  track.ID(),
			})
		}
		err := client.SubscribeTracks(tracksReq)
		require.NoError(t, err)
	})

	client.OnTracksAdded(func(addedTracks []ITrack) {
		setTracks := make(map[string]TrackType, 0)
		for _, track := range addedTracks {
			setTracks[track.ID()] = TrackTypeMedia
		}
		client.SetTracksSourceType(setTracks)
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		glog.Info("test: on track", track.Msid())
	})

	return client, pc
}

func TestClientDataChannel(t *testing.T) {
	t.Parallel()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	ctx := testRoom.sfu.context

	pc1, _, _ := CreateDataPair(ctx, testRoom, roomManager.options.IceServers, "peer1")

	dcChan := make(chan *webrtc.DataChannel)
	pc1.OnDataChannel(func(c *webrtc.DataChannel) {
		dcChan <- c
	})

	timeout, cancelTimeout := context.WithTimeout(ctx, 10*time.Second)

	defer cancelTimeout()

	select {
	case <-timeout.Done():
		t.Fatal("timeout waiting for data channel")
	case dc := <-dcChan:
		require.Equal(t, "internal", dc.Label())
	}
}
