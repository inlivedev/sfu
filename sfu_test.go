package sfu

import (
	"context"
	"log"
	"strconv"
	"testing"
	"time"

	"github.com/inlivedev/sfu/testhelper"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

type PeerClient struct {
	PeerConnection *webrtc.PeerConnection
	ID             string
}

func TestActiveTracks(t *testing.T) {
	// _ = os.Setenv("PION_LOG_DEBUG", "pc,dtls")
	// _ = os.Setenv("PION_LOG_TRACE", "ice")
	// _ = os.Setenv("PIONS_LOG_INFO", "all")

	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	peerCount := 3
	trackCount := 0
	connectedCount := 0
	trackChan := make(chan *webrtc.TrackRemote)
	peerChan := make(chan PeerClient)
	connectedChan := make(chan bool)
	peers := make(map[string]PeerClient, 0)
	udpMux := NewUDPMux(ctx, 40004)

	sfu := setup(t, udpMux, ctx, peerCount, trackChan, peerChan, connectedChan)
	defer sfu.Stop()

	ctxTimeout, cancel := context.WithTimeout(ctx, 50*time.Second)

	defer cancel()

	expectedTracks := (peerCount * 2) * (peerCount - 1)
	log.Println("expected tracks: ", expectedTracks)
Loop:
	for {
		select {
		case <-ctxTimeout.Done():
			require.Equal(t, expectedTracks, trackCount)
			break Loop
		case <-connectedChan:
			connectedCount++
			log.Println("connected count: ", connectedCount)
		case <-trackChan:
			trackCount++
			log.Println("remote track count: ", trackCount)
			if trackCount == expectedTracks { // 2 clients
				require.Equal(t, expectedTracks, trackCount)
				break Loop
			}
		case client := <-peerChan:
			peers[client.ID] = client
			log.Println("peer count: ", len(peers))
		}
	}

	currentTrack := 0

	for _, client := range peers {
		for _, transceiver := range client.PeerConnection.GetTransceivers() {
			if transceiver != nil {
				currentTrack++
			}
		}
	}

	log.Println("current clients count: ", len(peers), ",current client tracks count: ", currentTrack, "peer tracks count: ", trackCount)

	stoppedClient := 0

	for _, client := range peers {
		isStopped := make(chan bool)
		relay := sfu.Clients[client.ID]

		relay.OnConnectionStateChanged(func(state webrtc.PeerConnectionState) {
			if state == webrtc.PeerConnectionStateClosed {
				isStopped <- true
			}
		})

		relay.Stop()

		timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		select {
		case <-timeout.Done():
			break
		case <-isStopped:
			stoppedClient++
		}

		if stoppedClient == 1 {
			break
		}
	}

	require.Equal(t, 1, stoppedClient)

	// count left tracks
	leftTracks := 0

	for _, client := range sfu.Clients {
		for _, receiver := range client.PeerConnection.GetReceivers() {
			if receiver.Track() != nil {
				leftTracks++
			}
		}
	}

	currentTrack = 0

	for _, peer := range peers {

		for _, transceiver := range peer.PeerConnection.GetTransceivers() {
			if transceiver != nil && transceiver.Receiver().Track() != nil {
				currentTrack++
			}
		}
	}

	log.Println("current tracks count: ", currentTrack)

	expectedLeftTracks := (len(sfu.Clients) * 2) * (len(sfu.Clients) - 1)
	log.Println("left tracks: ", leftTracks, "from clients: ", len(sfu.Clients))
	log.Println("expected left tracks: ", expectedLeftTracks)
	require.Equal(t, expectedLeftTracks, leftTracks)
}

// this test is to test if an SFU
func TestSFUShutdownOnNoClient(t *testing.T) {

}

func createPeer(ctx context.Context, t *testing.T, s *SFU, tracks []*webrtc.TrackLocalStaticSample, mediaEngine *webrtc.MediaEngine, connectedChan chan bool) (peerConnection *webrtc.PeerConnection, localTrackChan chan *webrtc.TrackRemote) {
	t.Helper()

	iceServers := []webrtc.ICEServer{}

	if s.turnServer.Host != "" {
		iceServers = append(iceServers,
			webrtc.ICEServer{
				URLs:           []string{"turn:" + s.turnServer.Host + ":" + strconv.Itoa(s.turnServer.Port)},
				Username:       s.turnServer.Username,
				Credential:     s.turnServer.Password,
				CredentialType: webrtc.ICECredentialTypePassword,
			},
			webrtc.ICEServer{
				URLs: []string{"stun:" + s.turnServer.Host + ":" + strconv.Itoa(s.turnServer.Port)},
			})
	}

	config := webrtc.Configuration{
		ICEServers: iceServers,
	}

	// Create a new RTCPeerConnection
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	peerConnection, err := api.NewPeerConnection(config)
	require.NoError(t, err)

	remoteTrack := make(chan *webrtc.TrackRemote)

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		remoteTrack <- track
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		if connectionState == webrtc.ICEConnectionStateConnected {
			log.Println("ICE connected")
			connectedChan <- true
		}
	})

	for _, track := range tracks {
		// track.OnEnded(func(err error) {
		// 	fmt.Printf("Track (ID: %s) ended with error: %v\n",
		// 		track.ID(), err)
		// })

		_, err = peerConnection.AddTrack(track)

		require.NoError(t, err)
	}

	offer, _ := peerConnection.CreateOffer(nil)

	err = peerConnection.SetLocalDescription(offer)
	require.NoError(t, err)

	gatheringComplete := webrtc.GatheringCompletePromise(peerConnection)
	localCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	select {
	case <-gatheringComplete:
	case <-localCtx.Done():
	}

	return peerConnection, remoteTrack
}

func setup(t *testing.T, udpMux *UDPMux, ctx context.Context, peerCount int, trackChan chan *webrtc.TrackRemote, peerChan chan PeerClient, connectedChan chan bool) *SFU {
	// test adding stream
	// Prepare the configuration
	// iceServers := []webrtc.ICEServer{{URLs: []string{
	// 	"stun:stun.l.google.com:19302",
	// }}}

	turn := TurnServer{
		Port:     3478,
		Host:     "turn.inlive.app",
		Username: "inlive",
		Password: "inlivesdkturn",
	}

	sfu := New(ctx, turn, udpMux)

	// tracks, mediaEngine := testhelper.GetTestTracks()
	for i := 0; i < peerCount; i++ {
		go func() {
			pendingCandidates := make([]*webrtc.ICECandidate, 0)
			receivedAnswer := false

			streamID := testhelper.GenerateSecureToken(16)
			peerTracks, mediaEngine := testhelper.GetStaticTracks(ctx, streamID)

			peer, remoteTrackChan := createPeer(ctx, t, sfu, peerTracks, mediaEngine, connectedChan)
			testhelper.SetPeerConnectionTracks(peer, peerTracks)

			uid := GenerateID([]int{sfu.Counter})
			peerChan <- PeerClient{
				PeerConnection: peer,
				ID:             uid,
			}
			relay := sfu.NewClient(uid, DefaultClientOptions())

			relay.OnRenegotiation = func(ctx context.Context, sdp webrtc.SessionDescription) webrtc.SessionDescription {
				if peer.SignalingState() != webrtc.SignalingStateClosed {
					_ = peer.SetRemoteDescription(sdp)
					answer, _ := peer.CreateAnswer(nil)
					_ = peer.SetLocalDescription(answer)

					for _, candidate := range pendingCandidates {
						err := peer.AddICECandidate(candidate.ToJSON())
						require.NoError(t, err)
					}

					log.Println("renegotiation triggered")
					return *peer.LocalDescription()
				}

				return webrtc.SessionDescription{}
			}

			relay.OnIceCandidate = func(ctx context.Context, candidate *webrtc.ICECandidate) {
				// log.Println("candidate: ", candidate.Address)

				if candidate != nil && receivedAnswer {
					// log.Println("adding candidate: ", candidate.Address)
					err := peer.AddICECandidate(candidate.ToJSON())
					require.NoError(t, err)
					return
				}

				pendingCandidates = append(pendingCandidates, candidate)
			}

			relay.Negotiate(*peer.LocalDescription())
			_ = peer.SetRemoteDescription(*relay.PeerConnection.LocalDescription())

			localCtx, cancelLocal := context.WithCancel(ctx)
			defer cancelLocal()

			for {
				select {
				case <-localCtx.Done():
					return
				case trackRemote := <-remoteTrackChan:
					trackChan <- trackRemote
				}
			}
		}()
	}

	return sfu
}
