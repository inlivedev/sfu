package sfu

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
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

func TestFlexFECSDP(t *testing.T) {
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

	// Create a peer connection that supports FlexFEC to simulate a client offering FlexFEC
	// The SFU should accept and include FlexFEC in its answer when EnableFlexFEC is true
	mediaEngine := GetMediaEngine()

	// Configure FlexFEC on the simulated client side to offer it
	i := &interceptor.Registry{}
	if err := webrtc.ConfigureFlexFEC03(126, mediaEngine, i); err != nil {
		t.Fatalf("Failed to configure FlexFEC on simulated client: %v", err)
	}

	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, i); err != nil {
		t.Fatalf("Failed to register interceptors on simulated client: %v", err)
	}

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i))
	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: DefaultTestIceServers(),
	})
	require.NoError(t, err)
	defer pc.Close()

	// Create client with FlexFEC enabled
	id := testRoom.CreateClientID()
	opts := DefaultClientOptions()
	opts.IceTrickle = true
	opts.EnableFlexFEC = true
	client, err := testRoom.AddClient(id, id, opts)
	require.NoError(t, err)

	// Setup ICE candidate handling
	clientPendingCandidates, pendingCandidates := exchangeICECandidates(pc, client)

	// Variables to store SDP for verification, written by the renegotiation callback
	var (
		sdpMu          sync.Mutex
		receivedOffer  webrtc.SessionDescription
		receivedAnswer webrtc.SessionDescription
	)

	client.onRenegotiation = func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		t.Log("Renegotiation triggered - inspecting SDP")

		if err := pc.SetRemoteDescription(offer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)

		sdpMu.Lock()
		receivedOffer = offer
		receivedAnswer = answer
		sdpMu.Unlock()

		if err := pc.SetLocalDescription(answer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		return *pc.LocalDescription(), nil
	}

	// Add video transceiver to trigger negotiation
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})

	// Override OnNegotiationNeeded to perform negotiation
	pc.OnNegotiationNeeded(func() {
		t.Log("OnNegotiationNeeded called")
		negotiate(pc, client, TestLogger, true)

		// Add pending candidates
		clientPendingCandidates.flush()
		pendingCandidates.flush()
	})

	// Wait for negotiation to complete
	timeout := time.After(15 * time.Second)
	for {
		select {
		case <-time.After(200 * time.Millisecond):
			sdpMu.Lock()
			received := receivedOffer.SDP != "" && receivedAnswer.SDP != ""
			sdpMu.Unlock()

			if received {
				goto sdpReceived
			}
		case <-timeout:
			t.Fatal("Timeout waiting for SDP offer and answer")
		case <-ctx.Done():
			t.Fatal("Test context cancelled")
		}
	}

sdpReceived:
	sdpMu.Lock()
	offerSDP, answerSDP := receivedOffer, receivedAnswer
	sdpMu.Unlock()

	// Verify the client offers FlexFEC (since we configured it to support FlexFEC)
	t.Logf("Client Offer SDP FlexFEC lines:\n%s", extractFlexFECLines(offerSDP.SDP))

	// The main test: Verify FlexFEC is present in the SFU's answer SDP
	// When a client offers FlexFEC and the SFU has EnableFlexFEC=true,
	// the SFU should accept and include FlexFEC in its answer
	assert.Contains(t, answerSDP.SDP, "a=rtpmap:126 flexfec-03", "Answer SDP should contain FlexFEC RTP map")
	assert.Contains(t, answerSDP.SDP, "a=fmtp:126", "Answer SDP should contain FlexFEC format parameters")
	t.Logf("FlexFEC successfully found in SFU answer SDP")

	// Log SDP content for verification
	t.Logf("SFU Answer SDP contains FlexFEC lines:\n%s", extractFlexFECLines(answerSDP.SDP))
}

// extractFlexFECLines extracts FlexFEC-related lines from SDP for debugging
func extractFlexFECLines(sdp string) string {
	lines := strings.Split(sdp, "\n")
	var flexfecLines []string
	for _, line := range lines {
		if strings.Contains(line, "126") && (strings.Contains(line, "flexfec") || strings.Contains(line, "rtpmap") || strings.Contains(line, "fmtp")) {
			flexfecLines = append(flexfecLines, line)
		}
	}
	return strings.Join(flexfecLines, "\n")
}

func TestOnNegotiationNeededCalledTwice(t *testing.T) {
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
	getNegotiationCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return negotiationCount
	}

	clientPendingCandidates, pendingCandidates := exchangeICECandidates(pc, client)

	client.onRenegotiation = func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		t.Log("Renegotiation triggered")

		if err := pc.SetRemoteDescription(offer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)
		if err := pc.SetLocalDescription(answer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		return *pc.LocalDescription(), nil
	}

	client.OnAllowedRemoteRenegotiation(func() {
		TestLogger.Infof("allowed remote renegotiation")

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
		clientPendingCandidates.flush()
		pendingCandidates.flush()
	})

	// Add track to trigger negotiation
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})

	// Wait until the first negotiation is done
	timeout := time.After(15 * time.Second)
	for {
		select {
		case <-time.After(200 * time.Millisecond):
			if count := getNegotiationCount(); count > 0 {
				t.Logf("Negotiation needed called %d times", count)
				goto negotiationDone
			}
		case <-timeout:
			t.Fatal("Timeout waiting for negotiation needed to be called")
		case <-ctx.Done():
			t.Fatal("Test context cancelled")
		}
	}

negotiationDone:

	// Add more tracks to re-trigger negotiation
	tracks, _ := GetStaticTracks(clientContext, clientContext, "test-peer", false)
	SetPeerConnectionTracks(clientContext, pc, tracks)

	// Wait for the second negotiation to be done
	timeout = time.After(15 * time.Second)
	for {
		select {
		case <-time.After(200 * time.Millisecond):
			if count := getNegotiationCount(); count > 1 {
				t.Logf("Negotiation needed called %d times", count)
				goto finalCheck
			}
		case <-timeout:
			t.Fatal("Timeout waiting for second negotiation needed to be called")
		case <-ctx.Done():
			t.Fatal("Test context cancelled")
		}
	}

finalCheck:

	// Verify OnNegotiationNeeded was called exactly once
	finalCount := getNegotiationCount()

	assert.GreaterOrEqual(t, finalCount, 2, "OnNegotiationNeeded should be called at least twice, but was called %d times", finalCount)
}

// pendingICECandidates queues ICE candidates for a peer until it has a remote description.
// The candidate callbacks and the negotiation callback run on different goroutines.
type pendingICECandidates struct {
	mu         sync.Mutex
	candidates []webrtc.ICECandidateInit
	hasRemote  func() bool
	add        func(webrtc.ICECandidateInit) error
}

func (p *pendingICECandidates) addOrQueue(candidate *webrtc.ICECandidate) {
	if candidate == nil {
		return
	}

	p.mu.Lock()
	if !p.hasRemote() {
		p.candidates = append(p.candidates, candidate.ToJSON())
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	_ = p.add(candidate.ToJSON())
}

// flush adds the queued candidates. Call it after the remote description is set.
func (p *pendingICECandidates) flush() {
	p.mu.Lock()
	candidates := p.candidates
	p.candidates = nil
	p.mu.Unlock()

	for _, candidate := range candidates {
		_ = p.add(candidate)
	}
}

// exchangeICECandidates trickles the candidates of pc and client to each other. It returns the
// queues for the client's candidates (added to pc) and pc's candidates (added to the client).
func exchangeICECandidates(pc *webrtc.PeerConnection, client *Client) (toPC, toClient *pendingICECandidates) {
	toPC = &pendingICECandidates{
		hasRemote: func() bool { return pc.RemoteDescription() != nil },
		add:       pc.AddICECandidate,
	}
	toClient = &pendingICECandidates{
		hasRemote: func() bool { return client.PeerConnection().PC().RemoteDescription() != nil },
		add:       func(c webrtc.ICECandidateInit) error { return client.PeerConnection().PC().AddICECandidate(c) },
	}

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		toPC.addOrQueue(candidate)
	})

	pc.OnICECandidate(toClient.addOrQueue)

	return toPC, toClient
}

// A remote client that is allowed to negotiate but never calls Negotiate() must not block
// the SFU renegotiation forever.
func TestUnusedRemoteNegotiationDoesNotBlockRenegotiation(t *testing.T) {
	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	roomManager := NewManager(ctx, "test", sfuOpts)
	defer roomManager.Close()

	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = &[]string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomManager.CreateRoomID(), "test-room", RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	defer testRoom.Close()

	trackChan := make(chan bool)

	pc0, client0, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "peer-0", true, false, true)
	pc0.PeerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		trackChan <- true
	})

	require.Eventually(t, func() bool {
		return client0.PeerConnection().PC().ConnectionState() == webrtc.PeerConnectionStateConnected &&
			!client0.isInRenegotiation.Load()
	}, 10*time.Second, 10*time.Millisecond)

	// take the remote negotiation slot and never use it
	require.True(t, client0.IsAllowNegotiation())

	pc1, client1, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "peer-1", true, false, true)

	timeout, cancelTimeout := context.WithTimeout(ctx, 4*remoteNegotiationTimeout)
	defer cancelTimeout()

	tracksReceived := 0
	for tracksReceived < 2 {
		select {
		case <-timeout.Done():
			require.FailNow(t, "peer-0 didn't receive peer-1 tracks", "received %d of 2 tracks", tracksReceived)
		case <-trackChan:
			tracksReceived++
		}
	}

	require.NoError(t, testRoom.StopClient(client0.ID()))
	require.NoError(t, testRoom.StopClient(client1.ID()))
	require.NoError(t, pc0.PeerConnection.Close())
	require.NoError(t, pc1.PeerConnection.Close())
}

// A track the remote client adds with addTrack (a new sendrecv media section) must be received even
// when the SFU has a sendonly transceiver for a subscribed track that isn't negotiated yet.
func TestNewRemoteTrackWithPendingSubscribedTrack(t *testing.T) {
	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	roomManager := NewManager(ctx, "test", sfuOpts)
	defer roomManager.Close()

	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = &[]string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomManager.CreateRoomID(), "test-room", RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	defer testRoom.Close()

	pc0, client0, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "peer-0", true, false, true)

	require.Eventually(t, func() bool {
		return client0.PeerConnection().PC().ConnectionState() == webrtc.PeerConnectionStateConnected &&
			len(client0.tracks.GetTracks()) == 2 &&
			!client0.isInRenegotiation.Load()
	}, 10*time.Second, 10*time.Millisecond)

	// hold the SFU renegotiation, so the tracks peer-0 subscribes to stay unnegotiated
	require.True(t, client0.IsAllowNegotiation())

	pc1, client1, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "peer-1", true, false, true)

	require.Eventually(t, func() bool {
		for _, transceiver := range client0.PeerConnection().PC().GetTransceivers() {
			if transceiver.Mid() == "" && transceiver.Kind() == webrtc.RTPCodecTypeVideo && transceiver.Direction() == webrtc.RTPTransceiverDirectionSendonly {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "peer-0 didn't subscribe to peer-1 video")

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(ctx)
	defer iceConnectedCtxCancel()
	newTrack, _ := GetStaticVideoTrack(ctx, iceConnectedCtx, GenerateSecureToken(), GenerateSecureToken(), true, "")
	iceConnectedCtxCancel()

	// AddTrack creates a sendrecv media section, the peer's negotiation needed handler sends the offer
	_, err = pc0.PeerConnection.AddTrack(newTrack)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(client0.tracks.GetTracks()) == 3
	}, remoteNegotiationTimeout, 10*time.Millisecond, "the SFU didn't receive the new peer-0 track")

	require.NoError(t, testRoom.StopClient(client0.ID()))
	require.NoError(t, testRoom.StopClient(client1.ID()))
	require.NoError(t, pc0.PeerConnection.Close())
	require.NoError(t, pc1.PeerConnection.Close())
}
