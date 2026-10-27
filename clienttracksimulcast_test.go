package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type rewriteResult struct {
	ok  bool
	seq uint16
	ts  uint32
}

func rewriteSimulcast(r *simulcastRewriter, layer QualityLevel, seq uint16, ts uint32, now time.Time) rewriteResult {
	p := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}}
	ok := r.rewrite(p, layer, now)

	return rewriteResult{ok: ok, seq: p.SequenceNumber, ts: p.Timestamp}
}

func TestSimulcastRewriterContiguousAcrossLayerSwitches(t *testing.T) {
	r := newSimulcastRewriter(90000)
	now := time.Unix(0, 0)

	// each layer has its own random sequence and timestamp space
	layers := map[QualityLevel]struct {
		seq uint16
		ts  uint32
	}{
		QualityLow:  {seq: 9000, ts: 1_000_000},
		QualityMid:  {seq: 65500, ts: 4_294_000_000},
		QualityHigh: {seq: 300, ts: 50},
	}

	var lastSeq uint16
	var lastTS uint32
	sent := 0

	for i, layer := range []QualityLevel{QualityLow, QualityHigh, QualityMid, QualityLow, QualityHigh} {
		// the other layers keep running while we forward this one
		base := layers[layer]
		offset := uint16(i * 200)

		for j := uint16(0); j < 100; j++ {
			now = now.Add(10 * time.Millisecond)
			res := rewriteSimulcast(r, layer, base.seq+offset+j, base.ts+uint32(offset+j)*900, now)
			require.True(t, res.ok)

			if sent > 0 {
				require.Equal(t, lastSeq+1, res.seq, "layer %d packet %d", layer, j)
				require.Greater(t, int32(res.ts-lastTS), int32(0), "timestamp must advance, layer %d packet %d", layer, j)
			}

			lastSeq, lastTS = res.seq, res.ts
			sent++
		}
	}
}

func TestSimulcastRewriterFirstPacketKeepsSequence(t *testing.T) {
	r := newSimulcastRewriter(90000)
	now := time.Unix(0, 0)

	require.False(t, r.hasStarted())

	res := rewriteSimulcast(r, QualityLow, 1234, 5678, now)
	require.True(t, res.ok)
	require.Equal(t, uint16(1234), res.seq)
	require.Equal(t, uint32(5678), res.ts)
	require.True(t, r.hasStarted())
}

func TestSimulcastRewriterRetransmissionFillsGap(t *testing.T) {
	r := newSimulcastRewriter(90000)
	now := time.Unix(0, 0)

	require.Equal(t, uint16(100), rewriteSimulcast(r, QualityLow, 100, 0, now).seq)
	// switch to high, 501 is lost on the way in
	require.Equal(t, uint16(101), rewriteSimulcast(r, QualityHigh, 500, 0, now).seq)
	require.Equal(t, uint16(103), rewriteSimulcast(r, QualityHigh, 502, 0, now).seq)

	// the retransmission lands in the hole instead of rewinding the stream
	res := rewriteSimulcast(r, QualityHigh, 501, 0, now)
	require.True(t, res.ok)
	require.Equal(t, uint16(102), res.seq)

	require.Equal(t, uint16(104), rewriteSimulcast(r, QualityHigh, 503, 0, now).seq)
}

func TestSimulcastRewriterDropsPacketsFromBeforeSwitch(t *testing.T) {
	r := newSimulcastRewriter(90000)
	now := time.Unix(0, 0)

	for seq := uint16(10); seq < 20; seq++ {
		require.True(t, rewriteSimulcast(r, QualityLow, seq, 0, now).ok)
	}

	require.Equal(t, uint16(20), rewriteSimulcast(r, QualityHigh, 7000, 0, now).seq)

	// a late or retransmitted high packet from before the switch would reuse
	// sequence numbers that were already sent for the low layer
	require.False(t, rewriteSimulcast(r, QualityHigh, 6999, 0, now).ok)
	require.False(t, rewriteSimulcast(r, QualityHigh, 6990, 0, now).ok)

	require.Equal(t, uint16(21), rewriteSimulcast(r, QualityHigh, 7001, 0, now).seq)
}

func TestSimulcastRewriterSequenceWrapAround(t *testing.T) {
	r := newSimulcastRewriter(90000)
	now := time.Unix(0, 0)

	require.Equal(t, uint16(65534), rewriteSimulcast(r, QualityLow, 65534, 0, now).seq)
	require.Equal(t, uint16(65535), rewriteSimulcast(r, QualityLow, 65535, 0, now).seq)
	require.Equal(t, uint16(0), rewriteSimulcast(r, QualityHigh, 65530, 0, now).seq)
	require.Equal(t, uint16(1), rewriteSimulcast(r, QualityHigh, 65531, 0, now).seq)
	require.Equal(t, uint16(2), rewriteSimulcast(r, QualityLow, 3, 0, now).seq)
}

func TestSimulcastRewriterTimestampFollowsWallClockOnSwitch(t *testing.T) {
	r := newSimulcastRewriter(90000)
	now := time.Unix(0, 0)

	require.Equal(t, uint32(1000), rewriteSimulcast(r, QualityLow, 1, 1000, now).ts)

	// same frame keeps the same timestamp
	require.Equal(t, uint32(1000), rewriteSimulcast(r, QualityLow, 2, 1000, now).ts)

	now = now.Add(33 * time.Millisecond)
	res := rewriteSimulcast(r, QualityHigh, 9000, 123456789, now)
	require.Equal(t, uint32(1000+33*90), res.ts)

	// later frames of the new layer keep their spacing
	res = rewriteSimulcast(r, QualityHigh, 9001, 123456789+3000, now)
	require.Equal(t, uint32(1000+33*90+3000), res.ts)
}
