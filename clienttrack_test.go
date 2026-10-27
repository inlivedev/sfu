package sfu

import (
	"testing"

	"github.com/inlivedev/sfu/pkg/packetmap"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/require"
)

type mappedPacket struct {
	sent    bool
	seq     uint16
	payload int
}

func TestClientTrackQualityNoneDropsWithoutGap(t *testing.T) {
	ct := &clientTrack{packetmap: &packetmap.Map{}}

	push := func(seq uint16, blank bool) mappedPacket {
		p := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: []byte{1, 2, 3}}
		sent := ct.mapSequence(p, blank)

		return mappedPacket{sent: sent, seq: p.SequenceNumber, payload: len(p.Payload)}
	}

	require.Equal(t, mappedPacket{true, 100, 3}, push(100, false))
	require.Equal(t, mappedPacket{true, 101, 3}, push(101, false))

	// video hidden: the packets are dropped, not sent with an empty payload
	for seq := uint16(102); seq < 110; seq++ {
		require.False(t, push(seq, true).sent, "seq %d", seq)
	}

	// visible again: the receiver sees no gap
	require.Equal(t, mappedPacket{true, 102, 3}, push(110, false))
	// 111 is lost upstream
	require.Equal(t, mappedPacket{true, 104, 3}, push(112, false))

	// a late packet can't be dropped without a gap, so it is sent empty in its slot
	require.Equal(t, mappedPacket{true, 103, 0}, push(111, true))
}

func TestScaleableClientTrackQualityNoneDropsWithoutGap(t *testing.T) {
	ct := &scaleableClientTrack{clientTrack: &clientTrack{packetmap: &packetmap.Map{}}}

	push := func(seq uint16, quality QualityLevel) mappedPacket {
		// base layer packet that starts and ends a frame (B and E set, no picture id)
		p := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: []byte{0x0C, 0x00, 0x00}}
		vp9Packet := &codecs.VP9Packet{}
		_, err := vp9Packet.Unmarshal(p.Payload)
		require.NoError(t, err)

		sent := ct.rewrite(p, vp9Packet, quality)

		return mappedPacket{sent: sent, seq: p.SequenceNumber, payload: len(p.Payload)}
	}

	require.Equal(t, mappedPacket{true, 100, 3}, push(100, QualityLowLow))
	require.Equal(t, mappedPacket{true, 101, 3}, push(101, QualityLowLow))

	for seq := uint16(102); seq < 110; seq++ {
		require.False(t, push(seq, QualityNone).sent, "seq %d", seq)
	}

	require.Equal(t, mappedPacket{true, 102, 3}, push(110, QualityLowLow))
	require.Equal(t, mappedPacket{true, 104, 3}, push(112, QualityLowLow))
	require.Equal(t, mappedPacket{true, 103, 0}, push(111, QualityNone))
}
