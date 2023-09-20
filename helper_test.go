package sfu

import (
	"github.com/pion/webrtc/v3"
)

type PeerClient struct {
	PeerConnection  *webrtc.PeerConnection
	PendingTracks   []*webrtc.TrackLocalStaticSample
	ID              string
	RelayClient     *Client
	NeedRenegotiate bool
	InRenegotiation bool
}

type RemoteTrackTest struct {
	Track  *webrtc.TrackRemote
	Client *PeerClient
}

func DefaultTestIceServers() []webrtc.ICEServer {
	return []webrtc.ICEServer{
		{
			URLs:           []string{"turn:127.0.0.1:3478", "stun:127.0.0.1:3478"},
			Username:       "user",
			Credential:     "pass",
			CredentialType: webrtc.ICECredentialTypePassword,
		},
	}
}
