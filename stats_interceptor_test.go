package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// TestClientSharedStatsInterceptor checks that the SFU client and pc.GetStats() read from the
// same stats interceptor, so inbound RTP stats are still reported by pc.GetStats() and the
// SFU's statsGetter sees the same streams with wire-level byte counts.
func TestClientSharedStatsInterceptor(t *testing.T) {
	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	roomManager := NewManager(ctx, "test", sfuOpts)
	defer roomManager.Close()

	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = &[]string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomManager.CreateRoomID(), "test-room", RoomTypeLocal, roomOpts)
	require.NoError(t, err)

	_, client, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "peer1", false, false, true)

	require.Eventually(t, func() bool {
		for _, s := range client.PeerConnection().PC().GetStats() {
			inbound, ok := s.(webrtc.InboundRTPStreamStats)
			if !ok || inbound.BytesReceived == 0 {
				continue
			}

			stat := client.statsGetter.Get(uint32(inbound.SSRC))
			if stat != nil && stat.InboundRTPStreamStats.HeaderBytesReceived > 0 &&
				receivedWireBytes(*stat) > stat.InboundRTPStreamStats.BytesReceived {
				return true
			}
		}

		return false
	}, 30*time.Second, 500*time.Millisecond, "expecting pc.GetStats() and the SFU stats getter to report the same inbound streams")
}
