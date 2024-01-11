package sfu

import (
	"slices"
	"testing"
)

func TestPush(t *testing.T) {
	p := newPacketCaches(1000)
	p.push(1, 2, 3)
	if p.tail != 1 {
		t.Error("tail is not 1")
	}
	if p.head != 0 {
		t.Error("head is not 0")
	}
	if p.caches[1].sequence != 1 {
		t.Error("sequence is not 1")
	}
	if p.caches[1].timestamp != 2 {
		t.Error("timestamp is not 2")
	}
	if p.caches[1].dropCounter != 3 {
		t.Error("dropCounter is not 3")
	}
}

func TestOverwrite(t *testing.T) {
	p := newPacketCaches(1000)
	for i := 0; i < 1100; i++ {
		p.push(uint16(i), uint32(i), uint16(i))
	}
	if p.tail != 100 {
		t.Error("tail is not 0 got ", p.tail, " head ", p.head)
	}
	if p.head != 101 {
		t.Error("head is not 1, got ", p.head, " tail ", p.tail)
	}

	if p.caches[0].sequence != 999 {
		t.Error("sequence is not 1000, got ", p.caches[0].sequence, " tail ", p.tail, " head ", p.head)
	}
	if p.caches[0].timestamp != 999 {
		t.Error("timestamp is not 1000, got ", p.caches[0].timestamp, " tail ", p.tail, " head ", p.head)
	}
	if p.caches[0].dropCounter != 999 {
		t.Error("dropCounter is not 1000, got ", p.caches[0].dropCounter, " tail ", p.tail, " head ", p.head)
	}
}

func TestGet(t *testing.T) {
	p := newPacketCaches(1000)
	for i := 0; i < 1100; i++ {
		p.push(uint16(i), uint32(i), uint16(i))
	}

	pkt, ok := p.getPacket(1099)
	if !ok {
		t.Error("packet not found")
	}

	if pkt.sequence != 1099 {
		t.Error("sequence is not 1099, got ", pkt.sequence)
	}

	if pkt.timestamp != 1099 {
		t.Error("timestamp is not 1099, got ", pkt.timestamp)
	}

	if pkt.dropCounter != 1099 {
		t.Error("dropCounter is not 1099, got ", pkt.dropCounter)
	}

	pkt, ok = p.getPacket(1299)
	if ok {
		t.Error("packet found, expected not found")
	}

	pkt, ok = p.getPacket(99)
	if ok {
		t.Error("packet found, expected not found")
	}
}

func TestGetPacketOrBefore(t *testing.T) {
	// drop 8 packets
	dropPackets := []uint16{111, 222, 333, 444, 555, 666, 777, 888}
	p := newPacketCaches(1000)
	for i := 0; i < 1100; i++ {
		if !slices.Contains(dropPackets, uint16(i)) {
			p.push(uint16(i), uint32(i), uint16(i))
		}
	}

	if p.tail != 92 {
		t.Error("tail is not 92, got ", p.tail)
	}

	pkt, ok := p.getPacket(999)
	if !ok {
		t.Error("packet not found, expected found")
	}

	if pkt.sequence != 999 {
		t.Error("sequence is not 999, got ", pkt.sequence)
	}

	pkt, ok = p.getPacket(777)
	if ok {
		t.Error("packet found, expected not found")
	}

	pkt, ok = p.getPacketOrBefore(777)
	if !ok {
		t.Error("packet not found, expected found")
	}

	if pkt.sequence != 776 {
		t.Error("sequence is not 776, got ", pkt.sequence)
	}

}
