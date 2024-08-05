package sfu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

func TestLeaveRoom(t *testing.T) {
	// t.Parallel()

	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test", sfuOpts)

	defer roomManager.Close()

	roomID := roomManager.CreateRoomID()
	roomName := "test-leave-room-room"

	peerCount := 5

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = &[]string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)

	trackChan := make(chan bool)

	clients := make([]*Client, 0)

	for i := 0; i < peerCount; i++ {
		go func(i int) {
			pc, client, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), fmt.Sprintf("peer-%d", i), true, false)

			clients = append(clients, client)

			pc.PeerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
				trackChan <- true
			})
		}(i)
	}

	timeout, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	defer cancelTimeout()

	trackReceived := 0
	peerToClose := 2
	expectedTracks := (peerCount * 2) * (peerCount - 1)
	leftClientsCounts := peerCount - peerToClose
	expectedActiveTracks := (leftClientsCounts * 2) * (leftClientsCounts - 1)
	checkReceiverChan := make(chan bool)
	activeTracks := 0

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop

		case <-trackChan:
			trackReceived++
			t.Log("Tracks received: ", trackReceived, " from expected: ", expectedTracks)
			if trackReceived == expectedTracks {
				// put in go routine so the channel won't blocking
				go func() {
					// when all tracks received, make two peers leave the room
					t.Logf("all tracks received, make two peers leave the room")
					for i := 0; i < 2; i++ {
						err = testRoom.StopClient(clients[i].ID())
						require.NoError(t, err, "error stopping client: %v", err)
					}
					checkReceiverChan <- true
					t.Logf("two clients left the room")
				}()
			}

		case <-checkReceiverChan:
			go func() {
				ctxx, cancelx := context.WithCancel(ctx)
				defer cancelx()
				for {
					ticker := time.NewTicker(1 * time.Millisecond)
					select {
					case <-ctxx.Done():
						return
					case <-ticker.C:
						activeTracks = 0
						for _, client := range clients {
							for _, sender := range client.peerConnection.PC().GetSenders() {
								if sender.Track() != nil {
									activeTracks++
								}
							}
						}

						if activeTracks == expectedActiveTracks {
							cancelTimeout()
						}
					}
				}
			}()
		}

	}

	require.Equal(t, expectedTracks, trackReceived)
	require.Equal(t, activeTracks, expectedActiveTracks)
}

func TestRenegotiation(t *testing.T) {
	// t.Parallel()

	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test", sfuOpts)

	defer roomManager.Close()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	peerCount := 3

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = &[]string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)

	trackChan := make(chan bool)

	type Pair struct {
		pc     *webrtc.PeerConnection
		client *Client
	}

	pairs := make([]Pair, 0)

	for i := 0; i < peerCount; i++ {
		pc, client, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), fmt.Sprintf("peer-%d", i), true, false)

		pairs = append(pairs, Pair{pc.PeerConnection, client})

		client.OnTracksAdded(func(addedTracks []ITrack) {
			setTracks := make(map[string]TrackType, 0)
			for _, track := range addedTracks {
				setTracks[track.ID()] = TrackTypeMedia
			}
			client.SetTracksSourceType(setTracks)
		})

		pc.PeerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			trackChan <- true
		})
	}

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	trackReceived := 0
	expectedTracks := (peerCount * 2) * (peerCount - 1)
	expectedTracksAfterAdded := (peerCount * 3) * (peerCount - 1)

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop

		case <-trackChan:
			trackReceived++
			t.Log("Tracks received: ", trackReceived, " from expected: ", expectedTracks, "or after added: ", expectedTracksAfterAdded)
			if trackReceived == expectedTracks {
				go func() {
					// add more tracks to each clients
					for _, pair := range pairs {
						iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(ctx)
						defer iceConnectedCtxCancel()
						newTrack, _ := GetStaticVideoTrack(timeout, iceConnectedCtx, GenerateSecureToken(), GenerateSecureToken(), true, "")
						_, err := pair.pc.AddTransceiverFromTrack(newTrack)
						require.NoError(t, err, "error adding track: %v", err)
						negotiate(pair.pc, pair.client, TestLogger)
					}
				}()
			}

			if trackReceived == expectedTracksAfterAdded {
				break Loop
			}
		}
	}

	require.Equal(t, expectedTracksAfterAdded, trackReceived)
}
