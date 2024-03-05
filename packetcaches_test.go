package sfu

import (
	"fmt"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

var sortedNumbers = []uint16{65527, 65528, 65529, 65530, 65531, 65532, 65533, 65534, 65535, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
var unsortedNumbers = []uint16{65527, 65532, 65531, 65533, 65534, 65535, 1, 2, 3, 4, 5, 65528, 65529, 65530, 0, 6, 7, 8, 9, 10}

func TestAdd(t *testing.T) {
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

	caches := newPacketCaches(minLatency, maxLatency)

	for _, pkt := range packets {
		err := caches.Add(pkt)
		require.NoError(t, err)
	}

	require.Equal(t, caches.caches.Len(), len(sortedNumbers), "caches length should be equal to sortedNumbers length")

	i := 0
	for e := caches.caches.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*packetCache)
		require.Equal(t, packet.RTP.SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", packet.RTP.SequenceNumber, sortedNumbers[i]))
		i++
	}
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

	caches := newPacketCaches(minLatency, maxLatency)

	for _, pkt := range packets {
		err := caches.Add(pkt)
		require.NoError(t, err)
	}

	time.Sleep(5 * minLatency)

	sorted := caches.Flush()

	sorted = append(sorted, caches.Flush()...)

	require.Equal(t, len(sortedNumbers), len(sorted), "sorted length should be equal to sortedNumbers length")

	for i, pkt := range sorted {
		require.Equal(t, pkt.Header.SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", pkt.Header.SequenceNumber, sortedNumbers[i]))
	}
}

func TestSort(t *testing.T) {
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

	caches := newPacketCaches(minLatency, maxLatency)

	sorted := make([]*rtp.Packet, 0)
	packetAddedTime := make(map[uint16]time.Time, 0)
	packetLatencies := make(map[uint16]time.Duration, 0)

	resultsSeqs := make([]uint16, 0)

	for _, pkt := range unsortedPackets {
		sortedPackets, _ := caches.Sort(pkt)

		packetAddedTime[pkt.Header.SequenceNumber] = time.Now()

		sorted = append(sorted, sortedPackets...)

		for _, pkt := range sorted {
			resultsSeqs = append(resultsSeqs, pkt.Header.SequenceNumber)
			packetLatencies[pkt.Header.SequenceNumber] = time.Since(packetAddedTime[pkt.Header.SequenceNumber])
		}

		if pkt.Header.SequenceNumber == unsortedNumbers[0] {
			// first sort call should return immediately
			time.Sleep(2 * minLatency)
		} else if pkt.Header.SequenceNumber == unsortedNumbers[1] {
			require.Equal(t, 1, len(sorted), "sorted length should be equal to 1", resultsSeqs)
		} else if pkt.Header.SequenceNumber == 65530 {
			require.Equal(t, 1, len(sorted), "sorted length should be equal to 9, result ", resultsSeqs)
			time.Sleep(2 * minLatency)
		} else if pkt.Header.SequenceNumber == 0 {
			require.Equal(t, 15, len(sorted), "sorted length should be equal to 5, result ", resultsSeqs)
		}
	}

	time.Sleep(2 * minLatency)

	sortedPackets := caches.Flush()

	sorted = append(sorted, sortedPackets...)

	for _, pkt := range sorted {
		resultsSeqs = append(resultsSeqs, pkt.Header.SequenceNumber)
		packetLatencies[pkt.Header.SequenceNumber] = time.Since(packetAddedTime[pkt.Header.SequenceNumber])
	}

	require.Equal(t, len(sortedNumbers), len(sorted), "sorted length should be equal to sortedNumbers length")

	for seq, latency := range packetLatencies {
		if latency < minLatency {
			t.Error("packet sequence ", seq, " latency ", latency, " is less than minLatency ", minLatency)
		}
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

	caches := newPacketCaches(minLatency, maxLatency)

	sorted := make([]*rtp.Packet, 0)
	seqs := make([]uint16, 0)
	resultsSeqs := make([]uint16, 0)
	dropped := 0

	for _, pkt := range unsortedPackets {
		seqs = append(seqs, pkt.Header.SequenceNumber)

		if pkt.Header.SequenceNumber == 65535 {
			// last sort call should return immediately
			glog.Info("packet sequence ", pkt.Header.SequenceNumber)
			time.Sleep(2 * maxLatency)
			sortedPackets, err := caches.Sort(pkt)
			sorted = append(sorted, sortedPackets...)
			if err != nil {
				dropped++
			}
			for _, pkt := range sorted {
				resultsSeqs = append(resultsSeqs, pkt.Header.SequenceNumber)
			}
			require.Equal(t, 1, len(sorted), "sorted length should be equal to 1, result ", resultsSeqs, seqs)
		} else if pkt.Header.SequenceNumber == 0 {
			// last sort call should return immediately
			time.Sleep(2 * maxLatency)
			sortedPackets, err := caches.Sort(pkt)
			sorted = append(sorted, sortedPackets...)
			if err != nil {
				dropped++
			}
			for _, pkt := range sorted {
				resultsSeqs = append(resultsSeqs, pkt.Header.SequenceNumber)
			}
			// from 15 packets added, 3 packets will be dropped because it's too late
			require.Equal(t, 12, len(sorted), "sorted length should be equal to 15, result ", resultsSeqs, seqs)
		} else {
			sortedPackets, err := caches.Sort(pkt)
			sorted = append(sorted, sortedPackets...)
			if err != nil {
				dropped++
			}
		}
	}

	require.Equal(t, len(seqs)-dropped-5, len(sorted), "sorted length should be equal to 12, 3 packets still less than min latency. result ", resultsSeqs, seqs)

	time.Sleep(2 * minLatency)

	sortedPackets := caches.Flush()

	sorted = append(sorted, sortedPackets...)

	require.Equal(t, len(seqs)-dropped, len(sorted), "sorted length should be equal to 15, result ", resultsSeqs, seqs)
}
