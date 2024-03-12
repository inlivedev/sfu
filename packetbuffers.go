package sfu

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtp"
)

var (
	ErrPacketTooLate   = errors.New("packetbuffers: packet is too late")
	ErrPacketDuplicate = errors.New("packetbuffers: packet is duplicate")
)

// buffer ring for cached packets
type packetBuffers struct {
	init               bool
	mu                 sync.RWMutex
	buffers            *list.List
	lastSequenceNumber uint16
	// min duration to wait before sending
	minLatency time.Duration
	// max duration to wait before sending
	maxLatency   time.Duration
	oldestPacket *packet
	initSequence uint16
	packetCount  uint64
}

type packet struct {
	RTP       *rtp.Packet
	AddedTime time.Time
}

func newPacketBuffers(minLatency, maxLatency time.Duration) *packetBuffers {
	return &packetBuffers{
		mu:         sync.RWMutex{},
		buffers:    list.New(),
		minLatency: minLatency,
		maxLatency: maxLatency,
	}
}

func (p *packetBuffers) MaxLatency() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.maxLatency
}

func (p *packetBuffers) MinLatency() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.minLatency
}

func (p *packetBuffers) Add(pkt *rtp.Packet) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.init &&
		(p.lastSequenceNumber == pkt.SequenceNumber || IsRTPPacketLate(pkt.SequenceNumber, p.lastSequenceNumber)) {
		glog.Warning("packet cache: packet sequence ", pkt.SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber, ", will not adding the packet")

		return ErrPacketTooLate
	}

	newPacket := packet{
		RTP:       pkt,
		AddedTime: time.Now(),
	}

	var currentPkt *packet

	if p.buffers.Len() == 0 {
		p.buffers.PushBack(&newPacket)

		return nil
	}
	// add packet in order
Loop:
	for e := p.buffers.Back(); e != nil; e = e.Prev() {
		currentPkt = e.Value.(*packet)
		if currentPkt.RTP.SequenceNumber == pkt.SequenceNumber {
			glog.Warning("packet cache: packet sequence ", pkt.SequenceNumber, " already exists in the cache, will not adding the packet")

			return ErrPacketDuplicate
		}

		if currentPkt.RTP.SequenceNumber < pkt.SequenceNumber && pkt.SequenceNumber-currentPkt.RTP.SequenceNumber < uint16SizeHalf {
			p.buffers.InsertAfter(&newPacket, e)

			break Loop
		} else if currentPkt.RTP.SequenceNumber-pkt.SequenceNumber > uint16SizeHalf {
			p.buffers.InsertAfter(&newPacket, e)

			break Loop
		} else if e.Prev() == nil {
			p.buffers.PushFront(&newPacket)

			break Loop
		}
	}

	return nil
}

func (p *packetBuffers) pop(el *list.Element) *rtp.Packet {
	pkt := el.Value.(*packet)

	// make sure packet is not late
	if IsRTPPacketLate(pkt.RTP.SequenceNumber, p.lastSequenceNumber) {
		glog.Warning("packet cache: packet sequence ", pkt.RTP.SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber)
	}

	if p.init && pkt.RTP.SequenceNumber > p.lastSequenceNumber && pkt.RTP.SequenceNumber-p.lastSequenceNumber > 1 {
		// make sure packet has no gap
		glog.Warning("packet cache: packet sequence ", pkt.RTP.SequenceNumber, " has a gap with last sent ", p.lastSequenceNumber)
	}

	p.lastSequenceNumber = pkt.RTP.SequenceNumber
	p.packetCount++

	if p.oldestPacket == nil || p.oldestPacket.RTP.SequenceNumber == pkt.RTP.SequenceNumber {
		// oldest packet will be remove, find the next oldest packet in the list
		p.oldestPacket = nil
		for e := el.Next(); e != nil; e = e.Next() {
			packet := e.Value.(*packet)
			if p.oldestPacket == nil || packet.AddedTime.Before(p.oldestPacket.AddedTime) {
				p.oldestPacket = packet
			}
		}

	}

	// remove the packets from the cache
	p.buffers.Remove(el)

	return pkt.RTP
}

func (p *packetBuffers) flush() []*rtp.Packet {
	p.mu.Lock()
	defer p.mu.Unlock()

	packets := make([]*rtp.Packet, 0)
Loop:
	for e := p.buffers.Front(); p.buffers.Front() != nil; e = p.buffers.Front() {
		pkt := p.fetch(e)
		if pkt != nil {
			packets = append(packets, pkt)
		} else {
			break Loop
		}
	}

	return packets
}

func (p *packetBuffers) fetch(e *list.Element) *rtp.Packet {
	currentPacket := e.Value.(*packet)
	currentSeq := currentPacket.RTP.SequenceNumber
	latency := time.Since(currentPacket.AddedTime)

	if !p.init && latency > p.minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*packet).RTP.SequenceNumber, currentSeq) {
		// first packet to send, but make sure we have the packet in order
		p.initSequence = currentSeq
		p.init = true
		return p.pop(e)
	} else if (p.lastSequenceNumber < currentSeq || p.lastSequenceNumber-currentSeq > uint16SizeHalf) && currentSeq-p.lastSequenceNumber == 1 &&
		(time.Since(currentPacket.AddedTime) > p.minLatency || (p.oldestPacket != nil && time.Since(p.oldestPacket.AddedTime) > p.maxLatency)) {
		// the current packet is in sequence with the last packet we popped and passed the min latency
		return p.pop(e)
	} else {
		// there is a gap between the last packet we popped and the current packet
		// we should wait for the next packet

		// but check with the latency if there is a packet pass the max latency
		packetLatency := time.Since(currentPacket.AddedTime)
		// glog.Info("packet latency: ", packetLatency, " gap: ", gap, " currentSeq: ", currentSeq, " nextSeq: ", nextSeq)
		if packetLatency > p.maxLatency {
			// we have waited too long, we should send the packets
			glog.Warning("packet cache: packet sequence ", currentPacket.RTP.SequenceNumber, " latency ", packetLatency, ", reached max latency ", p.maxLatency, ", will sending the packets")
			return p.pop(e)
		}
	}

	return nil
}

func (p *packetBuffers) Pop() *rtp.Packet {
	p.mu.RLock()
	defer p.mu.RUnlock()

	frontElement := p.buffers.Front()
	if frontElement == nil {
		return nil
	}

	return p.fetch(frontElement)
}

func (p *packetBuffers) Flush() []*rtp.Packet {
	return p.flush()
}

func (p *packetBuffers) Last() *packet {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.buffers.Len() == 0 {
		return nil
	}

	return p.buffers.Back().Value.(*packet)
}

func (p *packetBuffers) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.buffers.Len()
}

func (p *packetBuffers) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for e := p.buffers.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*packet)
		packet.RTP = nil
		p.buffers.Remove(e)
	}

	p.buffers.Init()

	p.oldestPacket = nil
}
