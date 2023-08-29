package sfu

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/testhelper"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

type PeerClient struct {
	PeerConnection  *webrtc.PeerConnection
	PendingTracks   []*webrtc.TrackLocalStaticSample
	ID              string
	RelayClient     *Client
	NeedRenegotiate bool
	InRenegotiation bool
}

type RemoteTrack struct {
	Track  *webrtc.TrackRemote
	Client *PeerClient
}

func CreatePeer(ctx context.Context, t *testing.T, iceServers []webrtc.ICEServer, tracks []*webrtc.TrackLocalStaticSample, mediaEngine *webrtc.MediaEngine, connectedChan chan bool) (peerConnection *webrtc.PeerConnection, localTrackChan chan *webrtc.TrackRemote) {
	t.Helper()

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
			glog.Info("ICE connected")
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

func Setup(t *testing.T, ctx context.Context, udpMux *UDPMux, peerCount int, trackChan chan *RemoteTrack, peerChan chan *PeerClient, connectedChan chan bool) *SFU {
	// test adding stream
	// Prepare the configuration
	// iceServers := []webrtc.ICEServer{{URLs: []string{
	// 	"stun:stun.l.google.com:19302",
	// }}}
	t.Helper()

	turn := TurnServer{
		Port:     3478,
		Host:     "turn.inlive.app",
		Username: "inlive",
		Password: "inlivesdkturn",
	}

	iceServers := []webrtc.ICEServer{}

	if turn.Host != "" {
		iceServers = append(iceServers,
			webrtc.ICEServer{
				URLs:           []string{"turn:" + turn.Host + ":" + strconv.Itoa(turn.Port)},
				Username:       turn.Username,
				Credential:     turn.Password,
				CredentialType: webrtc.ICECredentialTypePassword,
			},
			webrtc.ICEServer{
				URLs: []string{"stun:" + turn.Host + ":" + strconv.Itoa(turn.Port)},
			})
	}

	s := New(ctx, turn, udpMux)

	// tracks, mediaEngine := testhelper.GetTestTracks()
	for i := 0; i < peerCount; i++ {
		go func() {
			pendingCandidates := make([]*webrtc.ICECandidate, 0)
			receivedAnswer := false

			peerTracks, mediaEngine, _ := testhelper.GetStaticTracks(ctx, fmt.Sprintf("stream-%d", i), true)

			peer, remoteTrackChan := CreatePeer(ctx, t, iceServers, peerTracks, mediaEngine, connectedChan)
			testhelper.SetPeerConnectionTracks(ctx, peer, peerTracks)

			uid := GenerateID([]int{s.Counter})

			peer.OnSignalingStateChange(func(state webrtc.SignalingState) {
				glog.Info("test: peer signaling state: ", uid, state)
			})

			relay := s.NewClient(uid, DefaultClientOptions())
			peerClient := &PeerClient{
				PeerConnection: peer,
				ID:             uid,
				RelayClient:    relay,
				PendingTracks:  make([]*webrtc.TrackLocalStaticSample, 0),
			}
			peerChan <- peerClient

			relay.OnTracksAdded = func(tracks []*Track) {
				setTracks := make(map[string]TrackType, 0)
				for _, track := range tracks {
					setTracks[track.ID()] = TrackTypeMedia
				}
				relay.SetTracksSourceType(setTracks)
			}

			relay.OnTracksAvailable = func(tracks []*Track) {
				req := make([]SubscribeTrackRequest, 0)
				for _, track := range tracks {
					req = append(req, SubscribeTrackRequest{
						ClientID: track.ClientID,
						TrackID:  track.LocalStaticRTP.ID(),
						StreamID: track.LocalStaticRTP.StreamID(),
					})
				}
				err := relay.SubscribeTracks(req)
				require.NoError(t, err)
			}

			relay.OnRenegotiation = func(ctx context.Context, sdp webrtc.SessionDescription) (webrtc.SessionDescription, error) {
				glog.Info("test: renegotiation triggered", peerClient.ID, CountTracks(peer.LocalDescription().SDP), peerClient.InRenegotiation)
				if peer.SignalingState() != webrtc.SignalingStateClosed {
					if peerClient.InRenegotiation {
						glog.Info("test: rollback renegotiation", peerClient.ID)
						_ = peer.SetLocalDescription(webrtc.SessionDescription{
							Type: webrtc.SDPTypeRollback,
						})
					}

					_ = peer.SetRemoteDescription(sdp)
					peerClient.InRenegotiation = false

					answer, _ := peer.CreateAnswer(nil)
					_ = peer.SetLocalDescription(answer)

					for _, candidate := range pendingCandidates {
						err := peer.AddICECandidate(candidate.ToJSON())
						require.NoError(t, err)
					}

					return *peer.LocalDescription(), nil
				}

				return webrtc.SessionDescription{}, errors.New("peer closed")
			}

			relay.OnIceCandidate = func(ctx context.Context, candidate *webrtc.ICECandidate) {
				// glog.Info("candidate: ", candidate.Address)

				if candidate != nil && receivedAnswer {
					// glog.Info("adding candidate: ", candidate.Address)
					err := peer.AddICECandidate(candidate.ToJSON())
					require.NoError(t, err)
					return
				}

				pendingCandidates = append(pendingCandidates, candidate)
			}

			relay.OnAllowedRemoteRenegotiation = func() {
				for _, track := range peerClient.PendingTracks {
					_, err := peer.AddTrack(track)
					require.NoError(t, err)
				}

				// reset pending tracks once processed
				peerClient.PendingTracks = make([]*webrtc.TrackLocalStaticSample, 0)

				glog.Error("test: renegotiating allowed for client: ", relay.ID)
				offer, _ := peer.CreateOffer(nil)
				err := peer.SetLocalDescription(offer)
				if err != nil {
					glog.Error("test: error setting local description: ", relay.ID, err)
				}
				require.NoError(t, err)
				answer, err := relay.Negotiate(*peer.LocalDescription())
				require.NoError(t, err)
				err = peer.SetRemoteDescription(*answer)
				if err != nil {
					glog.Info("test: error setting remote description: ", relay.ID, err)
				}
				require.NoError(t, err)
			}

			relay.Negotiate(*peer.LocalDescription())

			_ = peer.SetRemoteDescription(*relay.GetPeerConnection().LocalDescription())

			localCtx, cancelLocal := context.WithCancel(ctx)
			defer cancelLocal()

			for {
				select {
				case <-localCtx.Done():
					return
				case trackRemote := <-remoteTrackChan:
					trackChan <- &RemoteTrack{
						Track:  trackRemote,
						Client: peerClient,
					}
				}
			}
		}()
	}

	return s
}
