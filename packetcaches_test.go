package sfu

import (
	"slices"
	"testing"
)

func TestPush(t *testing.T) {
	p := newPacketCaches(1000)
	p.Push(1, 0, 0, 0, 0)
	p.Push(2, 0, 0, 0, 0)
	p.Push(3, 0, 0, 0, 0)

	if p.caches.Front().Value.(cachedPacket).sequence != 1 {
		t.Error("front is not 1")
	}
	if p.caches.Back().Value.(cachedPacket).sequence != 3 {
		t.Error("back is not 3")
	}

}

func TestOverwrite(t *testing.T) {
	p := newPacketCaches(1000)
	for i := 0; i < 1100; i++ {
		p.Push(uint16(i), uint32(i), uint16(i), 0, 0)
	}

}

func TestGet(t *testing.T) {
	p := newPacketCaches(1000)
	for i := 0; i < 1100; i++ {
		p.Push(uint16(i), uint32(i), uint16(i), uint8(0), uint8(0))
	}

	pkt, ok := p.GetPacket(1099)
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

	pkt, ok = p.GetPacket(1299)
	if ok {
		t.Error("packet found, expected not found")
	}

	pkt, ok = p.GetPacket(99)
	if ok {
		t.Error("packet found, expected not found")
	}
}

func TestGetPacketOrBefore(t *testing.T) {
	// drop 8 packets
	dropPackets := []uint16{65111, 65222, 65333, 65444, 1, 11, 222, 333, 444}
	p := newPacketCaches(1000)

	for i := 65035; i < 65536; i++ {
		if !slices.Contains(dropPackets, uint16(i)) {
			p.Push(uint16(i), uint32(i), uint16(i), uint8(0), uint8(0))
		}
	}

	for i := 1; i < 500; i++ {
		if !slices.Contains(dropPackets, uint16(i)) {
			p.Push(uint16(i), uint32(i), uint16(i), uint8(0), uint8(0))
		}
	}

	pkt, ok := p.GetPacketOrBefore(444)
	if !ok {
		t.Error("packet not found, expected found")
	}

	if pkt.sequence != 443 {
		t.Error("sequence is not 444, got ", pkt.sequence)
	}

	pkt, ok = p.GetPacketOrBefore(1)
	if !ok {
		t.Error("packet not found, expected found")
	}

	if pkt.sequence != 65535 {
		t.Error("sequence is not 65535, got ", pkt.sequence)
	}

}
