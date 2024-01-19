package sfu

import "sync"

// buffer ring for cached packets
type packetCaches struct {
	mu     sync.RWMutex
	head   uint16
	tail   uint16
	caches []cachedPacket
}

type cachedPacket struct {
	sequence    uint16
	timestamp   uint32
	dropCounter uint16
}

func newPacketCaches(size int) *packetCaches {
	return &packetCaches{
		mu:     sync.RWMutex{},
		head:   0,
		tail:   0,
		caches: make([]cachedPacket, size),
	}
}

func (p *packetCaches) push(sequence uint16, timestamp uint32, dropCounter uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.tail++

	if p.tail == uint16(len(p.caches)) {
		p.tail = 0
	}

	p.caches[p.tail] = cachedPacket{
		sequence:    sequence,
		timestamp:   timestamp,
		dropCounter: dropCounter,
	}

	if p.head == uint16(len(p.caches)) {
		p.head = 0
	}

	if p.tail == p.head {
		p.head++
	}
}

func (p *packetCaches) getPacket(sequence uint16) (cachedPacket, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.tail == p.head {
		return cachedPacket{}, false
	}

	for i := p.tail; i != p.head; i-- {
		if p.caches[i].sequence == sequence {
			return p.caches[i], true
		} else if p.caches[i].sequence < sequence {
			return cachedPacket{}, false
		}

		if i == 0 {
			i = uint16(len(p.caches) - 1)
		}
	}

	return cachedPacket{}, false
}

func (p *packetCaches) getPacketOrBefore(sequence uint16) (cachedPacket, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.tail == p.head {
		return cachedPacket{}, false
	}

	for i := p.tail; i != p.head; i-- {
		currentSeq := p.caches[i].sequence
		if currentSeq == sequence {
			return p.caches[i], true
		} else if currentSeq < sequence {
			if i == p.head {
				return cachedPacket{}, false
			} else {
				nextPacket := p.caches[i]
				return nextPacket, true
			}
		}

		if i == 0 {
			i = uint16(len(p.caches) - 1)
		}
	}

	return cachedPacket{}, false
}

func (p *packetCaches) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.head = 0
	p.tail = 0
}
