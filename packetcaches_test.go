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

	maxLatency := 100 * time.Millisecond

	caches := newPacketCaches(maxLatency)

	for _, pkt := range packets {
		caches.Add(pkt)
	}

	require.Equal(t, caches.caches.Len(), len(sortedNumbers), "caches length should be equal to sortedNumbers length")

	i := 0
	for e := caches.caches.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*packetCache)
		require.Equal(t, packet.Header.SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", packet.Header.SequenceNumber, sortedNumbers[i]))
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

	maxLatency := 100 * time.Millisecond

	caches := newPacketCaches(maxLatency)

	for _, pkt := range packets {
		caches.Add(pkt)
	}

	sorted := caches.Flush()

	sorted = append(sorted, caches.Flush()...)

	require.Equal(t, len(sortedNumbers), len(sorted), "sorted length should be equal to sortedNumbers length")

	for i, pkt := range sorted {
		require.Equal(t, pkt.Header.SequenceNumber, sortedNumbers[i], fmt.Sprintf("packet sequence number %d should be equal to sortedNumbers sequence number %d", pkt.Header.SequenceNumber, sortedNumbers[i]))
	}
}

func TestSort(t *testing.T) {
	t.Parallel()

	unsoretedPackets := make([]*rtp.Packet, len(unsortedNumbers))

	for i, seq := range unsortedNumbers {
		unsoretedPackets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		}
	}

	maxLatency := 100 * time.Millisecond

	caches := newPacketCaches(maxLatency)

	sorted := make([]*rtp.Packet, 0)

	for _, pkt := range unsoretedPackets {
		if pkt.Header.SequenceNumber == 65530 {
			glog.Info("packet sequence number ", pkt.Header.SequenceNumber)
		}
		resultsSeqs := make([]uint16, 0)
		sorted = append(sorted, caches.Sort(pkt)...)

		for _, pkt := range sorted {
			resultsSeqs = append(resultsSeqs, pkt.Header.SequenceNumber)
		}

		if pkt.Header.SequenceNumber == unsortedNumbers[0] {
			// first sort call should return immediately
			require.Equal(t, 1, len(sorted), "sorted length should be equal to 1", resultsSeqs)
		} else if pkt.Header.SequenceNumber == 65530 {
			// last sort call should return immediately
			require.Equal(t, 9, len(sorted), "sorted length should be equal to 9, result ", resultsSeqs)
		} else if pkt.Header.SequenceNumber == 0 {
			// last sort call should return immediately
			require.Equal(t, 15, len(sorted), "sorted length should be equal to 5, result ", resultsSeqs)
		}
	}

	require.Equal(t, len(sortedNumbers), len(sorted), "sorted length should be equal to sortedNumbers length")
}
