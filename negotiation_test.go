package sfu

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSinglePeerConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create room manager and room
	sfuOpts := DefaultOptions()
	roomManager := NewManager(ctx, "test", sfuOpts)
	defer roomManager.Close()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"
	roomOpts := DefaultRoomOptions()

	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	defer testRoom.Close()

	// Create a single peer pair
	pc, client, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "test-peer", false, false, true)
	defer pc.PeerConnection.Close()

	// Wait for connection to be established
	connectionStateChanged := make(chan webrtc.PeerConnectionState, 10)
	pc.PeerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		t.Logf("Connection state changed to: %s", state)
		select {
		case connectionStateChanged <- state:
		default:
		}
	})

	// Wait for connected state or timeout
	timeout := time.After(15 * time.Second)
	for {
		select {
		case state := <-connectionStateChanged:
			switch state {
			case webrtc.PeerConnectionStateConnected:
				t.Log("Peer connection established successfully")
				goto connected
			case webrtc.PeerConnectionStateFailed:
				t.Fatal("Peer connection failed")
			default:
				t.Logf("Current connection state: %s", state)
			}
		case <-timeout:
			currentState := pc.PeerConnection.ConnectionState()
			t.Fatalf("Timeout waiting for connection, current state: %s", currentState)
		case <-ctx.Done():
			t.Fatal("Test context cancelled")
		}
	}

connected:
	// Verify client is active
	time.Sleep(1 * time.Second) // Allow some time for the client to be set up
	assert.NotNil(t, client)
	assert.Equal(t, ClientStateActive, client.state.Load())
}

func TestOnNegotiationNeededCalledOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create room manager and room
	sfuOpts := DefaultOptions()
	roomManager := NewManager(ctx, "test", sfuOpts)
	defer roomManager.Close()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"
	roomOpts := DefaultRoomOptions()

	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	defer testRoom.Close()

	// Create a peer connection manually to override OnNegotiationNeeded
	clientContext, cancelClient := context.WithCancel(ctx)
	defer cancelClient()

	mediaEngine := GetMediaEngine()
	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: DefaultTestIceServers(),
	})
	require.NoError(t, err)
	defer pc.Close()

	// Create client
	id := testRoom.CreateClientID()
	opts := DefaultClientOptions()
	opts.IceTrickle = true
	client, err := testRoom.AddClient(id, id, opts)
	require.NoError(t, err)

	// Counter for OnNegotiationNeeded calls
	negotiationCount := 0
	var mu sync.Mutex

	clientPendingCandidates := make([]*webrtc.ICECandidate, 0, 10)
	pendingCandidates := make([]*webrtc.ICECandidate, 0, 10)
	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		if pc.RemoteDescription() == nil {
			clientPendingCandidates = append(clientPendingCandidates, candidate)
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		if client.PeerConnection().pc.RemoteDescription() == nil {
			pendingCandidates = append(pendingCandidates, candidate)
			return
		}

		err = client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
	})

	client.onRenegotiation = func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		t.Log("Renegotiation triggered")

		err = pc.SetRemoteDescription(offer)
		if err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)
		err = pc.SetLocalDescription(answer)
		if err != nil {
			return webrtc.SessionDescription{}, err
		}

		return *pc.LocalDescription(), nil
	}

	client.OnAllowedRemoteRenegotiation(func() {
		TestLogger.Infof("allowed remote renegotiation")
		negotiate(pc, client, TestLogger, true)
	})

	// Override OnNegotiationNeeded to count calls and still perform negotiation
	pc.OnNegotiationNeeded(func() {
		mu.Lock()
		negotiationCount++
		currentCount := negotiationCount
		mu.Unlock()
		t.Logf("OnNegotiationNeeded called %d times", currentCount)

		// Still need to call negotiate to perform the actual negotiation
		negotiate(pc, client, TestLogger, true)
		if len(clientPendingCandidates) > 0 {
			for _, candidate := range clientPendingCandidates {
				_ = pc.AddICECandidate(candidate.ToJSON())
			}
			clientPendingCandidates = nil
		}
		if len(pendingCandidates) > 0 {
			for _, candidate := range pendingCandidates {
				_ = client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
			}
			pendingCandidates = nil
		}
	})

	// Add tracks to trigger negotiation
	tracks, _ := GetStaticTracks(clientContext, clientContext, "test-peer", false)
	SetPeerConnectionTracks(clientContext, pc, tracks)

	// Wait a bit for any potential additional negotiation calls
	time.Sleep(3 * time.Second)

	// Verify OnNegotiationNeeded was called exactly once
	mu.Lock()
	finalCount := negotiationCount
	mu.Unlock()

	assert.Equal(t, 1, finalCount, "OnNegotiationNeeded should be called exactly once, but was called %d times", finalCount)
}
