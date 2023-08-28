package sfu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestTracksManualSubscribe(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test-join-left", Options{WebRTCPort: 40003})

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	peerCount := 5

	// create new room
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal)
	require.NoError(t, err, "error creating room: %v", err)

	tracksAvailableChan := make(chan int)
	trackChan := make(chan bool)

	for i := 0; i < peerCount; i++ {
		pc, client, _, _ := createPeerPair(t, ctx, testRoom, fmt.Sprintf("peer-%d", i), true)
		client.OnTracksAvailable = func(availableTracks []*Track) {
			tracksAvailableChan <- len(availableTracks)
			tracksReq := make([]SubscribeTrackRequest, 0)
			for _, track := range availableTracks {
				tracksReq = append(tracksReq, SubscribeTrackRequest{
					ClientID: track.ClientID,
					TrackID:  track.LocalStaticRTP.ID(),
					StreamID: track.LocalStaticRTP.StreamID(),
				})
			}
			err := client.SubscribeTracks(tracksReq)
			require.NoError(t, err)
		}

		client.OnTracksAdded = func(addedTracks []*Track) {
			setTracks := make(map[string]TrackType, 0)
			for _, track := range addedTracks {
				setTracks[track.ID()] = TrackTypeMedia
			}
			client.SetTracksSourceType(setTracks)
		}

		pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			trackChan <- true
		})
	}

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	tracksAdded := 0
	trackReceived := 0
	expectedTracks := (peerCount * 2) * (peerCount - 1)

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case added := <-tracksAvailableChan:
			tracksAdded += added

			// if tracksAdded == peerCounts*2 {
			// 	break Loop
			// }
		case <-trackChan:
			trackReceived++
			if trackReceived == expectedTracks {
				break Loop
			}
		}

	}

	require.Equal(t, peerCount*peerCount*2, tracksAdded)
	require.Equal(t, expectedTracks, trackReceived)
}

func TestAutoSubscribeTracks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test-join-left", Options{WebRTCPort: 40004})

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	peerCount := 5

	// create new room
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal)
	require.NoError(t, err, "error creating room: %v", err)

	trackChan := make(chan bool)

	for i := 0; i < peerCount; i++ {
		pc, client, _, _ := createPeerPair(t, ctx, testRoom, fmt.Sprintf("peer-%d", i), true)
		client.SubscribeAllTracks()

		client.OnTracksAdded = func(addedTracks []*Track) {
			setTracks := make(map[string]TrackType, 0)
			for _, track := range addedTracks {
				setTracks[track.ID()] = TrackTypeMedia
			}
			client.SetTracksSourceType(setTracks)
		}

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
