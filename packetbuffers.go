package sfu

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/rtppool"
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
	maxLatency           time.Duration
	oldestPacket         *rtppool.RetainablePacket
	initSequence         uint16
	packetCount          uint64
	waitTimeMu           sync.RWMutex
	waitTimes            []time.Duration
	lastSequenceWaitTime uint16
	waitTimeResetCounter uint16
}

const waitTimeSize = 5000

func newPacketBuffers(minLatency, maxLatency time.Duration) *packetBuffers {
	return &packetBuffers{
		mu:                   sync.RWMutex{},
		buffers:              list.New(),
		minLatency:           minLatency,
		maxLatency:           maxLatency,
		waitTimeMu:           sync.RWMutex{},
		waitTimes:            make([]time.Duration, waitTimeSize),
		lastSequenceWaitTime: 0,
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

	newPacket := rtppool.NewPacket(&pkt.Header, pkt.Payload)

	if p.buffers.Len() == 0 {
		p.buffers.PushBack(newPacket)

		return nil
	}
	// add packet in order
Loop:
	for e := p.buffers.Back(); e != nil; e = e.Prev() {
		currentPkt := e.Value.(*rtppool.RetainablePacket)
		if currentPkt.Header().SequenceNumber == pkt.SequenceNumber {
			// glog.Warning("packet cache: packet sequence ", pkt.SequenceNumber, " already exists in the cache, will not adding the packet")

			return ErrPacketDuplicate
		}

		if currentPkt.Header().SequenceNumber < pkt.SequenceNumber && pkt.SequenceNumber-currentPkt.Header().SequenceNumber < uint16SizeHalf {
			p.buffers.InsertAfter(newPacket, e)

			break Loop
		} else if currentPkt.Header().SequenceNumber-pkt.SequenceNumber > uint16SizeHalf {
			p.buffers.InsertAfter(newPacket, e)

			break Loop
		} else if e.Prev() == nil {
			p.buffers.PushFront(newPacket)

			break Loop
		}
	}

	return nil
}

func (p *packetBuffers) pop(el *list.Element) *rtppool.RetainablePacket {
	defer func() {
		go p.checkWaitTimeAdjuster()
	}()

	pkt := el.Value.(*rtppool.RetainablePacket)

	// make sure packet is not late
	if IsRTPPacketLate(pkt.Header().SequenceNumber, p.lastSequenceNumber) {
		glog.Warning("packet cache: packet sequence ", pkt.Header().SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber)
	}

	if p.init && pkt.Header().SequenceNumber > p.lastSequenceNumber && pkt.Header().SequenceNumber-p.lastSequenceNumber > 1 {
		// make sure packet has no gap
		glog.Warning("packet cache: packet sequence ", pkt.Header().SequenceNumber, " has a gap with last sent ", p.lastSequenceNumber)
	}

	p.lastSequenceNumber = pkt.Header().SequenceNumber
	p.packetCount++

	if p.oldestPacket == nil || p.oldestPacket.Header().SequenceNumber == pkt.Header().SequenceNumber {
		// oldest packet will be remove, find the next oldest packet in the list
		p.oldestPacket = nil
		for e := el.Next(); e != nil; e = e.Next() {
			packet := e.Value.(*rtppool.RetainablePacket)
			if p.oldestPacket == nil || packet.AddedTime().Before(p.oldestPacket.AddedTime()) {
				p.oldestPacket = packet
			}
		}

	}

	// remove the packets from the cache
	p.buffers.Remove(el)

	return pkt
}

func (p *packetBuffers) flush() []*rtppool.RetainablePacket {
	p.mu.Lock()
	defer p.mu.Unlock()

	packets := make([]*rtppool.RetainablePacket, 0)
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

func (p *packetBuffers) fetch(e *list.Element) *rtppool.RetainablePacket {
	currentPacket := e.Value.(*rtppool.RetainablePacket)

	currentSeq := currentPacket.Header().SequenceNumber
	latency := time.Since(currentPacket.AddedTime())

	if p.oldestPacket != nil && time.Since(p.oldestPacket.AddedTime()) > p.maxLatency {
		// we have waited too long, we should send the packets
		return p.pop(e)
	}

	if !p.init && latency > p.minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*rtppool.RetainablePacket).Header().SequenceNumber, currentSeq) {
		// first packet to send, but make sure we have the packet in order
		p.initSequence = currentSeq
		p.init = true
		return p.pop(e)
	}

	if (p.lastSequenceNumber < currentSeq || p.lastSequenceNumber-currentSeq > uint16SizeHalf) && currentSeq-p.lastSequenceNumber == 1 {
		// the current packet is in sequence with the last packet we popped

		// we record the wait time the packet getting reordered
		p.recordWaitTime(e)

		if time.Since(currentPacket.AddedTime()) > p.minLatency {
			// passed the min latency
			return p.pop(e)
		}

	}

	// there is a gap between the last packet we popped and the current packet
	// we should wait for the next packet

	// but check with the latency if there is a packet pass the max latency
	packetLatency := time.Since(currentPacket.AddedTime())
	// glog.Info("packet latency: ", packetLatency, " gap: ", gap, " currentSeq: ", currentSeq, " nextSeq: ", nextSeq)
	if packetLatency > p.maxLatency {
		// we have waited too long, we should send the packets
		glog.Warning("packet cache: packet sequence ", currentPacket.Header().SequenceNumber, " latency ", packetLatency, ", reached max latency ", p.maxLatency, ", will sending the packets")
		return p.pop(e)
	}

	return nil
}

func (p *packetBuffers) Pop() *rtppool.RetainablePacket {
	p.mu.RLock()
	defer p.mu.RUnlock()

	frontElement := p.buffers.Front()
	if frontElement == nil {
		return nil
	}

	return p.fetch(frontElement)
}

func (p *packetBuffers) Flush() []*rtppool.RetainablePacket {
	return p.flush()
}

func (p *packetBuffers) Last() *rtppool.RetainablePacket {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.buffers.Len() == 0 {
		return nil
	}

	return p.buffers.Back().Value.(*rtppool.RetainablePacket)
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
		packet := e.Value.(*rtppool.RetainablePacket)
		packet.Release()
		p.buffers.Remove(e)

	}

	p.buffers.Init()

	p.oldestPacket = nil
}

func (p *packetBuffers) recordWaitTime(el *list.Element) {
	p.waitTimeMu.Lock()
	defer p.waitTimeMu.Unlock()

	pkt := el.Value.(*rtppool.RetainablePacket)

	if p.lastSequenceWaitTime == pkt.Header().SequenceNumber || IsRTPPacketLate(pkt.Header().SequenceNumber, p.lastSequenceWaitTime) {
		// don't record late packet or already recorded packet
		return
	}

	p.lastSequenceWaitTime = pkt.Header().SequenceNumber

	// remove oldest packet from the wait times if more than 500
	if uint16(len(p.waitTimes)+1) > waitTimeSize {
		p.waitTimes = p.waitTimes[1:]
	}

	p.waitTimes = append(p.waitTimes, time.Since(pkt.AddedTime()))

}

func (p *packetBuffers) checkWaitTimeAdjuster() {
	if p.waitTimeResetCounter > waitTimeSize {
		p.waitTimeMu.Lock()
		defer p.waitTimeMu.Unlock()

		// calculate the average wait time
		var totalWaitTime time.Duration
		for _, wt := range p.waitTimes {
			totalWaitTime += wt
		}

		averageWaitTime := totalWaitTime / time.Duration(len(p.waitTimes))

		if averageWaitTime > p.minLatency {
			// increase the min latency
			p.minLatency = averageWaitTime
			glog.Info("packet cache: average wait time ", averageWaitTime, ", increasing min latency to ", p.minLatency)
		} else if averageWaitTime < p.minLatency {
			// decrease the min latency
			p.minLatency = averageWaitTime
			glog.Info("packet cache: average wait time ", averageWaitTime, ", decreasing min latency to ", p.minLatency)
		}

		p.waitTimeResetCounter = 0
	} else {
		p.waitTimeResetCounter++
	}
}
