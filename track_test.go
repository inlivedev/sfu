package sfu

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// TestVoiceActivityDetection publishes two audio tracks from one peer and checks each gets its voice detector.
// The VAD interceptor registers a detector per stream while OnTrack looks them up, so run it with -race too.
func TestVoiceActivityDetection(t *testing.T) {
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

	pc, client, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "peer-vad", true, false, true)
	require.True(t, client.IsVADEnabled())

	audioTracks := func() []*AudioTrack {
		tracks := make([]*AudioTrack, 0)
		for _, track := range client.Tracks() {
			if audioTrack, ok := track.(*AudioTrack); ok {
				tracks = append(tracks, audioTrack)
			}
		}

		return tracks
	}

	require.Eventually(t, func() bool {
		return len(audioTracks()) == 1
	}, 30*time.Second, 100*time.Millisecond, "publisher's first audio track was not added")

	// a second audio track on the same peer, bound by the SFU in a renegotiation
	secondAudio, _ := GetStaticAudioTrack(ctx, ctx, GenerateSecureToken(), "peer-vad", true)
	SetPeerConnectionTracks(ctx, pc.PeerConnection, []*webrtc.TrackLocalStaticSample{secondAudio})

	require.Eventually(t, func() bool {
		return len(audioTracks()) == 2
	}, 30*time.Second, 100*time.Millisecond, "publisher's audio tracks were not added")

	for _, audioTrack := range audioTracks() {
		audioTrack.mu.Lock()
		vad := audioTrack.vad
		audioTrack.mu.Unlock()

		require.NotNil(t, vad, "audio track %s has no voice detector", audioTrack.ID())
		require.Equal(t, uint32(audioTrack.SSRC()), vad.SSRC())
	}

	require.NoError(t, testRoom.StopClient(client.ID()))
	require.NoError(t, pc.PeerConnection.Close())
}

// TestVoiceDetectorsConcurrentBind binds audio streams on the VAD interceptor, as Pion does during a negotiation,
// while another goroutine looks the detectors up, as OnTrack does for a stream bound earlier.
func TestVoiceDetectorsConcurrentBind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vads := newVoiceDetectors()

	factory := voiceactivedetector.NewInterceptor(ctx, TestLogger)
	factory.OnNew(func(i *voiceactivedetector.Interceptor) {
		i.OnNewVAD(vads.set)
	})

	vadInterceptor, err := factory.NewInterceptor("")
	require.NoError(t, err)

	const streams = 50

	reader := interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		return 0, a, nil
	})

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		for ssrc := uint32(1); ssrc <= streams; ssrc++ {
			vadInterceptor.BindRemoteStream(&interceptor.StreamInfo{SSRC: ssrc, MimeType: webrtc.MimeTypeOpus, ClockRate: 48000}, reader)
		}
	}()

	go func() {
		defer wg.Done()

		for ssrc := uint32(1); ssrc <= streams; ssrc++ {
			_, _ = vads.get(ssrc)
		}
	}()

	wg.Wait()

	for ssrc := uint32(1); ssrc <= streams; ssrc++ {
		vad, ok := vads.get(ssrc)
		require.True(t, ok, "no voice detector for SSRC %d", ssrc)
		require.Equal(t, ssrc, vad.SSRC())
	}
}

func createPeerAudio(ctx context.Context, room *Room, iceServers []webrtc.ICEServer, peerName string) (*webrtc.PeerConnection, *Client, chan *webrtc.TrackRemote) {
	var (
		client      *Client
		mediaEngine *webrtc.MediaEngine = GetMediaEngine()
	)

	if len(iceServers) == 0 {
		iceServers = []webrtc.ICEServer{
			{
				URLs:           []string{"stun:stun.l.google.com:19302", "stun:127.0.0.1:3478"},
				Username:       "user",
				Credential:     "pass",
				CredentialType: webrtc.ICECredentialTypePassword,
			},
		}
	}

	i := &interceptor.Registry{}

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, i); err != nil {
		panic(err)
	}

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i))

	pc, _ := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	trackChan := make(chan *webrtc.TrackRemote)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		trackChan <- track
	})

	audioTrack, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")

	_, err := pc.AddTrack(audioTrack)
	if err != nil {
		panic(err)
	}

	go sendAudioPackets(ctx, audioTrack)

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	id := room.CreateClientID()

	opts := DefaultClientOptions()

	opts.EnableVoiceDetection = true

	client, _ = room.AddClient(id, id, opts)

	client.OnAllowedRemoteRenegotiation(func() {
		TestLogger.Info("allowed remote renegotiation")
		negotiate(pc, client, TestLogger, true)
	})

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.state.Load() == ClientStateEnded {
			TestLogger.Info("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		currentTranscv := len(pc.GetTransceivers())

		TestLogger.Infof("test: got renegotiation ", peerName)
		defer TestLogger.Infof("test: renegotiation done ", peerName)
		if err := pc.SetRemoteDescription(offer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)

		if err := pc.SetLocalDescription(answer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		newTcv := len(pc.GetTransceivers()) - currentTranscv
		TestLogger.Infof("test: new transceiver ", newTcv, " total tscv ", len(pc.GetTransceivers()))

		return *pc.LocalDescription(), nil
	})

	negotiate(pc, client, TestLogger, true)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
	})

	return pc, client, trackChan
}

func sendAudioPackets(ctx context.Context, track *webrtc.TrackLocalStaticRTP) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := track.WriteRTP(&rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i),
					SSRC:           1,
				},
				Payload: []byte{0x00},
			}); err != nil {
				TestLogger.Errorf("error writing rtp packet: ", err.Error())
				return
			}
		}

		i++

	}
}
