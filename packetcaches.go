package sfu

import (
	"container/list"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtp"
)

var ErrPacketTooLate = errors.New("packet is too late")

// buffer ring for cached packets
type packetCaches struct {
	init               bool
	size               int
	mu                 sync.RWMutex
	caches             *list.List
	lastSequenceNumber uint16
	maxLatency         time.Duration
}

type packetCache struct {
	RTP       *rtp.Packet
	AddedTime time.Time
}

func newPacketCaches(maxLatency time.Duration) *packetCaches {
	return &packetCaches{
		mu:         sync.RWMutex{},
		caches:     list.New(),
		maxLatency: maxLatency,
	}
}

// Sort sorts the packets and returns the sorted packets ASAP
func (p *packetCaches) Sort(pkt *rtp.Packet) ([]*rtp.Packet, error) {
	err := p.Add(pkt)

	packets := make([]*rtp.Packet, 0)

	packets = append(packets, p.flush()...)

	return packets, err
}

func (p *packetCaches) Add(pkt *rtp.Packet) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.init && (p.lastSequenceNumber > pkt.SequenceNumber && pkt.SequenceNumber-p.lastSequenceNumber > math.MaxUint16/2) {
		glog.Warning("packet cache: packet sequence ", pkt.SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber, ", will not adding the packet")
		return ErrPacketTooLate
	}

	newPacket := packetCache{
		RTP:       pkt,
		AddedTime: time.Now(),
	}

	var packet *packetCache

	if p.caches.Len() == 0 {
		p.caches.PushBack(&newPacket)
		return nil
	}
	// add packet in order
Loop:
	for e := p.caches.Back(); e != nil; e = e.Prev() {
		packet = e.Value.(*packetCache)
		if packet.RTP.SequenceNumber == pkt.SequenceNumber {
			return nil
		}

		if packet.RTP.SequenceNumber < pkt.SequenceNumber && pkt.SequenceNumber-packet.RTP.SequenceNumber < 30000 {
			p.caches.InsertAfter(&newPacket, e)
			break Loop
		} else if packet.RTP.SequenceNumber-pkt.SequenceNumber > math.MaxUint16/2 {
			p.caches.InsertAfter(&newPacket, e)
			break Loop
		} else if e.Prev() == nil {
			p.caches.PushFront(&newPacket)
			break Loop
		}
	}

	return nil
}

func (p *packetCaches) appendPacket(packets []*rtp.Packet, e *list.Element) []*rtp.Packet {
	pkt := e.Value.(*packetCache)
	p.lastSequenceNumber = pkt.RTP.SequenceNumber

	packets = append(packets, pkt.RTP)

	// remove the packets from the cache
	p.caches.Remove(e)

	return packets
}

func (p *packetCaches) flush() []*rtp.Packet {
	p.mu.RLock()
	defer p.mu.RUnlock()

	defer func() {
		if !p.init {
			p.init = true
		}
	}()

	packets := make([]*rtp.Packet, 0)
Loop:
	for e := p.caches.Front(); p.caches.Front() != nil; e = p.caches.Front() {
		currentPacket := e.Value.(*packetCache)
		currentSeq := currentPacket.RTP.SequenceNumber

		if !p.init {
			// first packet to send, return immediately
			packets = p.appendPacket(packets, e)
			break Loop
		} else if (p.lastSequenceNumber < currentSeq || p.lastSequenceNumber-currentSeq > math.MaxUint16/2) && currentSeq-p.lastSequenceNumber == 1 {
			// the current packet is in sequence with the last packet we popped
			packets = p.appendPacket(packets, e)
		} else {
			// there is a gap between the last packet we popped and the current packet
			// we should wait for the next packet

			// but check with the latency if there is a packet pass the max latency
			packetLatency := time.Since(currentPacket.AddedTime)
			// glog.Info("packet latency: ", packetLatency, " gap: ", gap, " currentSeq: ", currentSeq, " nextSeq: ", nextSeq)
			if packetLatency > p.maxLatency {
				// we have waited too long, we should send the packets
				glog.Warning("packet cache: packet sequence ", currentPacket.RTP.SequenceNumber, " latency ", packetLatency, ", reached max latency ", p.maxLatency, ", will sending the packets")
				packets = p.appendPacket(packets, e)

			} else {
				// we should wait for the next packet
				break Loop
			}

		}
	}

	return packets
}

func (p *packetCaches) Flush() []*rtp.Packet {
	return p.flush()
}

func (p *packetCaches) Last() *packetCache {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.caches.Len() == 0 {
		return nil
	}

	return p.caches.Back().Value.(*packetCache)
}

func (p *packetCaches) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.caches.Len()
}

func (p *packetCaches) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.caches.Init()
}
