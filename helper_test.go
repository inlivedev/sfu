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
	return []webrtc.ICEServer{}
}
