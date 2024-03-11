package sfu

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/glog"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

func TestVoiceActivityDetection(t *testing.T) {

}

func createPeerAudio(ctx context.Context, room *Room, iceServers []webrtc.ICEServer, peerName string) (*webrtc.PeerConnection, *Client, chan *webrtc.TrackRemote) {
	var (
		client      *Client
		mediaEngine *webrtc.MediaEngine = GetMediaEngine()
	)

	if len(iceServers) == 0 {
		iceServers = []webrtc.ICEServer{
			{
				URLs:           []string{"turn:127.0.0.1:3478", "stun:127.0.0.1:3478"},
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

	client.SubscribeAllTracks()

	client.OnAllowedRemoteRenegotiation(func() {
		glog.Info("allowed remote renegotiation")
		negotiate(pc, client)
	})

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.state.Load() == ClientStateEnded {
			glog.Info("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		currentTranscv := len(pc.GetTransceivers())

		glog.Info("test: got renegotiation ", peerName)
		defer glog.Info("test: renegotiation done ", peerName)
		if err := pc.SetRemoteDescription(offer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)

		if err := pc.SetLocalDescription(answer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		newTcv := len(pc.GetTransceivers()) - currentTranscv
		glog.Info("test: new transceiver ", newTcv, " total tscv ", len(pc.GetTransceivers()))

		return *pc.LocalDescription(), nil
	})

	negotiate(pc, client)

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
				glog.Error("error writing rtp packet: ", err.Error())
				return
			}
		}

		i++

	}
}
