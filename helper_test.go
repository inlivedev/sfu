package sfu

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/testhelper"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
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

func createPeerPair(t *testing.T, ctx context.Context, testRoom *Room, peerName string, loop bool) (*webrtc.PeerConnection, *Client, stats.Getter, chan bool) {
	t.Helper()
	clientContext, cancelClient := context.WithCancel(ctx)
	var client *Client

	iceServers := []webrtc.ICEServer{
		{
			URLs:           []string{"turn:127.0.0.1:3478", "stun:127.0.0.1:3478"},
			Username:       "user",
			Credential:     "pass",
			CredentialType: webrtc.ICECredentialTypePassword,
		},
	}
	tracks, mediaEngine, done := testhelper.GetStaticTracks(clientContext, peerName, loop)

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

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			glog.Info("test: peer connection closed ", peerName)
			if client != nil {
				_ = testRoom.StopClient(client.ID)
				cancelClient()
			}
		}

	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		go func() {
			ctxx, cancell := context.WithCancel(clientContext)
			defer cancell()

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

		for {
			select {
			case <-clientContext.Done():
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
		defer glog.Info("test: renegotiation done ", peerName)
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
