package sfu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

var sortedNumbers = []uint16{65526, 65527, 65528, 65529, 65530, 65531, 65532, 65533, 65534, 65535, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

var unsortedNumbers = []uint16{65526, 65527, 65527, 65526, 65532, 65531, 65533, 65534, 65535, 1, 2, 3, 4, 5, 65528, 65529, 65530, 0, 6, 7, 8, 9, 10}

var pool = rtppool.New()

func TestAdd(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	packets := make([]*rtp.Packet, len(unsortedNumbers))

	for i, seq := range unsortedNumbers {
		packets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		}
	}

	minLatency := 10 * time.Millisecond
	maxLatency := 100 * time.Millisecond

	caches := NewPacketBuffers(ctx, minLatency, maxLatency, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	for i, pkt := range packets {
		rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
		err := caches.Add(rp)
		if i != 2 && i != 3 {
			require.NoError(t, err)
		}
	}

	require.Equal(t, caches.buffers.Len(), len(sortedNumbers), "caches length should be equal to sortedNumbers length")

	i := 0
	for e := caches.buffers.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*rtppool.RetainablePacket)
		require.Equal(t, packet.Header().SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", packet.Header().SequenceNumber, sortedNumbers[i]))
		i++
		packet.Release()
	}
}

func TestAddLost(t *testing.T) {
	t.Parallel()

	packets := make([]*rtp.Packet, len(unsortedNumbers))

	for i, seq := range unsortedNumbers {
		packets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		}
	}

	minLatency := 10 * time.Millisecond
	maxLatency := 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caches := NewPacketBuffers(ctx, minLatency, maxLatency, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	for i, pkt := range packets {
		if pkt.SequenceNumber == 65533 {
			// drop packet 65533
			continue
		}
		rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
		err := caches.Add(rp)

		if i != 2 && i != 3 {
			require.NoError(t, err)
		}
	}

	require.Equal(t, caches.buffers.Len(), len(sortedNumbers)-1, "caches length should be equal to sortedNumbers length")

	i := 0
	for e := caches.buffers.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*rtppool.RetainablePacket)
		if sortedNumbers[i] == 65533 {
			i++
		}

		require.Equal(t, packet.Header().SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", packet.Header().SequenceNumber, sortedNumbers[i]))
		i++
		packet.Release()
	}
}

func TestDuplicateAdd(t *testing.T) {
	t.Parallel()

	packets := make([]*rtp.Packet, len(unsortedNumbers))

	for i, seq := range unsortedNumbers {
		packets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		}
	}

	minLatency := 10 * time.Millisecond
	maxLatency := 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caches := NewPacketBuffers(ctx, minLatency, maxLatency, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	for i, pkt := range packets {
		if i == 9 {
			t.Log("packet sequence ", pkt.Header.SequenceNumber)
		}

		rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
		err := caches.Add(rp)
		if i == 2 || i == 3 {
			require.EqualError(t, err, ErrPacketTooLate.Error())
		} else {
			require.NoError(t, err, "should not return error on add packet %d sequence %d", i, pkt.Header.SequenceNumber)
		}

		if i == 1 {
			time.Sleep(2 * minLatency)
			pkts := caches.Flush()
			require.Equal(t, 2, len(pkts), "caches length should be equal to 1")
		}
	}

	// require.Equal(t, caches.buffers.Len(), len(sortedNumbers), "caches length should be equal to sortedNumbers length")

	// i := 0
	// for e := caches.buffers.Front(); e != nil; e = e.Next() {
	// 	packet := e.Value.(*Packet)
	// 	require.Equal(t, packet.RTP.Header().SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", packet.RTP.Header().SequenceNumber, sortedNumbers[i]))
	// 	i++
	// }
}
func TestFlush(t *testing.T) {
	t.Parallel()

	packets := make([]*rtp.Packet, len(unsortedNumbers))

	for i, seq := range unsortedNumbers {
		packets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		}
	}

	minLatency := 10 * time.Millisecond
	maxLatency := 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caches := NewPacketBuffers(ctx, minLatency, maxLatency, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	for i, pkt := range packets {
		rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
		err := caches.Add(rp)
		if i != 2 && i != 3 {
			require.NoError(t, err)
		}
	}

	time.Sleep(5 * minLatency)

	sorted := caches.Flush()

	sorted = append(sorted, caches.Flush()...)

	require.Equal(t, len(sortedNumbers), len(sorted), "sorted length should be equal to sortedNumbers length")

	for i, pkt := range sorted {
		require.Equal(t, pkt.Packet.Header().SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", pkt.Packet.Header().SequenceNumber, sortedNumbers[i]))
	}
}

func TestFlushBetweenAdded(t *testing.T) {
	t.Parallel()

	packets := make([]*rtp.Packet, len(unsortedNumbers))

	for i, seq := range unsortedNumbers {
		packets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		}
	}

	minLatency := 10 * time.Millisecond
	maxLatency := 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caches := NewPacketBuffers(ctx, minLatency, maxLatency, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	sorted := make([]*Packet, 0)

	for i, pkt := range packets {
		rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
		err := caches.Add(rp)
		if i != 2 && i != 3 {
			require.NoError(t, err)
		}

		if pkt.Header.SequenceNumber == 65535 ||
			pkt.Header.SequenceNumber == 65530 {
			time.Sleep(2 * minLatency)
			sorted = append(sorted, caches.Flush()...)
		}
	}

	time.Sleep(5 * minLatency)

	sorted = append(sorted, caches.Flush()...)

	require.Equal(t, len(sortedNumbers), len(sorted), "sorted length should be equal to sortedNumbers length")

	for i, pkt := range sorted {
		require.Equal(t, pkt.Packet.Header().SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", pkt.Packet.Header().SequenceNumber, sortedNumbers[i]))
	}
}

func TestLatency(t *testing.T) {
	t.Parallel()

	unsortedPackets := make([]*rtp.Packet, len(unsortedNumbers))

	for i, seq := range unsortedNumbers {
		unsortedPackets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		}
	}

	minLatency := 50 * time.Millisecond
	maxLatency := 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caches := NewPacketBuffers(ctx, minLatency, maxLatency, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	sorted := make([]*Packet, 0)
	seqs := make([]uint16, 0)
	resultsSeqs := make([]uint16, 0)
	dropped := 0

	for _, pkt := range unsortedPackets {
		seqs = append(seqs, pkt.Header.SequenceNumber)

		if pkt.Header.SequenceNumber == 65535 {
			// last sort call should return immediately
			t.Log("packet sequence ", pkt.Header.SequenceNumber)
			time.Sleep(2 * maxLatency)
			rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
			err := caches.Add(rp)
			sortedPackets := caches.Flush()
			sorted = append(sorted, sortedPackets...)
			if err != nil {
				dropped++
			}
			for _, pkt := range sorted {
				resultsSeqs = append(resultsSeqs, pkt.Packet.Header().SequenceNumber)
			}
			require.Equal(t, 6, len(sorted), "sorted length should be equal to 6, result ", resultsSeqs, seqs)
		} else if pkt.Header.SequenceNumber == 0 {
			// last sort call should return immediately
			time.Sleep(2 * maxLatency)
			rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
			err := caches.Add(rp)
			sortedPackets := caches.Flush()
			sorted = append(sorted, sortedPackets...)
			if err != nil {
				dropped++
			}
			for _, pkt := range sorted {
				resultsSeqs = append(resultsSeqs, pkt.Packet.Header().SequenceNumber)
			}
			// from 15 packets added, 3 packets will be dropped because it's too late
			require.Equal(t, 13, len(sorted), "sorted length should be equal to 13, result ", resultsSeqs, seqs)
		} else {
			rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
			err := caches.Add(rp)
			sortedPackets := caches.Flush()
			sorted = append(sorted, sortedPackets...)
			if err != nil {
				dropped++
			}
		}
	}

	require.Equal(t, len(seqs)-dropped-5, len(sorted), "sorted length should be equal to 13, 3 packets still less than min latency. result ", resultsSeqs, seqs)

	time.Sleep(2 * minLatency)

	sortedPackets := caches.Flush()

	sorted = append(sorted, sortedPackets...)

	require.Equal(t, len(seqs)-dropped, len(sorted), "sorted length should be equal to 15, result ", resultsSeqs, seqs)
}

func BenchmarkPushPool(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	testPackets := make([]*rtp.Packet, b.N)
	for i := 0; i < b.N; i++ {
		testPackets[i] = &rtp.Packet{
			Header:  rtp.Header{},
			Payload: make([]byte, 1400),
		}
	}

	packetBuffers := NewPacketBuffers(ctx, 10*time.Millisecond, 100*time.Millisecond, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pkt := testPackets[i]
		rp := pool.NewPacket(&pkt.Header, pkt.Payload, nil)
		_ = packetBuffers.Add(rp)
	}

}

func BenchmarkPopPool(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	packetBuffers := NewPacketBuffers(ctx, 10*time.Millisecond, 100*time.Millisecond, false, logging.NewDefaultLoggerFactory().NewLogger("sfu"))

	for i := 0; i < b.N; i++ {
		rp := pool.NewPacket(&rtp.Header{}, make([]byte, 1400), nil)
		packetBuffers.Add(rp)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {

		p := packetBuffers.Pop()
		if p != nil {
			rtp := pool.GetPacket()
			rtp.Header = *p.Packet.Header()
			rtp.Payload = p.Packet.Payload()

			// b.Logf("packet sequence %d", rtpPacket.SequenceNumber)
			p.Packet.Release()

			pool.PutPacket(rtp)
		}
	}

}
