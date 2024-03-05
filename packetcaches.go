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
	mu                 sync.RWMutex
	caches             *list.List
	lastSequenceNumber uint16
	// min duration to wait before sending
	minLatency time.Duration
	// max duration to wait before sending
	maxLatency   time.Duration
	oldestPacket *packetCache
}

type packetCache struct {
	RTP       *rtp.Packet
	AddedTime time.Time
}

func newPacketCaches(minLatency, maxLatency time.Duration) *packetCaches {
	return &packetCaches{
		mu:         sync.RWMutex{},
		caches:     list.New(),
		minLatency: minLatency,
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

func (p *packetCaches) appendPacket(packets []*rtp.Packet, el *list.Element) []*rtp.Packet {
	pkt := el.Value.(*packetCache)
	p.lastSequenceNumber = pkt.RTP.SequenceNumber

	packets = append(packets, pkt.RTP)

	if p.oldestPacket == nil || p.oldestPacket.RTP.SequenceNumber == pkt.RTP.SequenceNumber {
		// oldest packet will be remove, find the next oldest packet in the list
		p.oldestPacket = nil
		for e := el.Next(); e != nil; e = e.Next() {
			packet := e.Value.(*packetCache)
			if p.oldestPacket == nil || packet.AddedTime.Before(p.oldestPacket.AddedTime) {
				p.oldestPacket = packet
			}
		}

	}

	// remove the packets from the cache
	p.caches.Remove(el)

	return packets
}

func (p *packetCaches) flush() []*rtp.Packet {
	p.mu.RLock()
	defer p.mu.RUnlock()

	packets := make([]*rtp.Packet, 0)
Loop:
	for e := p.caches.Front(); p.caches.Front() != nil; e = p.caches.Front() {
		currentPacket := e.Value.(*packetCache)
		currentSeq := currentPacket.RTP.SequenceNumber

		if !p.init {
			// first packet to send, return immediately
			latency := time.Since(currentPacket.AddedTime)
			if latency > p.minLatency {
				packets = p.appendPacket(packets, e)
				p.init = true
			}

			break Loop
		} else if (p.lastSequenceNumber < currentSeq || p.lastSequenceNumber-currentSeq > math.MaxUint16/2) && currentSeq-p.lastSequenceNumber == 1 &&
			(time.Since(currentPacket.AddedTime) > p.minLatency || (p.oldestPacket != nil && time.Since(p.oldestPacket.AddedTime) > p.maxLatency)) {
			// the current packet is in sequence with the last packet we popped and passed the min latency

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
