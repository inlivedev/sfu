package fakeclient

import (
	"context"

	"github.com/inlivedev/sfu"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v3"
)

type FakeClient struct {
	ID             string
	PeerConnection *webrtc.PeerConnection
	Client         *sfu.Client
	Stats          stats.Getter
}

func Create(ctx context.Context, log logging.LeveledLogger, room *sfu.Room, iceServers []webrtc.ICEServer, id string, simulcast bool) *FakeClient {
	pc, client, stats, _ := sfu.CreatePeerPair(ctx, log, room, iceServers, id, true, simulcast)

	return &FakeClient{
		ID:             id,
		PeerConnection: pc,
		Client:         client,
		Stats:          stats,
	}
}
