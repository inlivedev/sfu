package sfu

import (
	"container/list"
	"errors"
	"sort"
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
	latencyMu          sync.RWMutex
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
		latencyMu:            sync.RWMutex{},
		minLatency:           minLatency,
		maxLatency:           maxLatency,
		waitTimeMu:           sync.RWMutex{},
		waitTimes:            make([]time.Duration, waitTimeSize),
		lastSequenceWaitTime: 0,
	}
}

func (p *packetBuffers) MaxLatency() time.Duration {
	p.latencyMu.RLock()
	defer p.latencyMu.RUnlock()
	return p.maxLatency
}

func (p *packetBuffers) MinLatency() time.Duration {
	p.latencyMu.RLock()
	defer p.latencyMu.RUnlock()
	return p.minLatency
}

func (p *packetBuffers) Add(pkt *rtp.Packet) error {
	p.mu.Lock()
	defer func() {
		p.checkPacketWaitTime()
		p.checkWaitTimeAdjuster()
		p.mu.Unlock()
	}()

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
	pkt := el.Value.(*rtppool.RetainablePacket)

	// make sure packet is not late
	if IsRTPPacketLate(pkt.Header().SequenceNumber, p.lastSequenceNumber) {
		glog.Warning("packet cache: packet sequence ", pkt.Header().SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber)
	}

	if p.init && pkt.Header().SequenceNumber > p.lastSequenceNumber && pkt.Header().SequenceNumber-p.lastSequenceNumber > 1 {
		// make sure packet has no gap
		glog.Warning("packet cache: packet sequence ", pkt.Header().SequenceNumber, " has a gap with last sent ", p.lastSequenceNumber)
	}

	p.mu.Lock()
	p.lastSequenceNumber = pkt.Header().SequenceNumber
	p.packetCount++
	p.mu.Unlock()

	if p.oldestPacket == nil || p.oldestPacket.Header().SequenceNumber == pkt.Header().SequenceNumber {
		// oldest packet will be remove, find the next oldest packet in the list
		p.oldestPacket = nil

		p.mu.RLock()
		for e := el.Next(); e != nil; e = e.Next() {
			packet := e.Value.(*rtppool.RetainablePacket)
			if p.oldestPacket == nil || packet.AddedTime().Before(p.oldestPacket.AddedTime()) {
				p.oldestPacket = packet
			}
		}
		p.mu.RUnlock()

	}

	// remove the packets from the cache
	p.mu.Lock()
	p.buffers.Remove(el)
	p.mu.Unlock()

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
	p.latencyMu.RLock()
	maxLatency := p.maxLatency
	minLatency := p.minLatency
	p.latencyMu.RUnlock()

	currentPacket := e.Value.(*rtppool.RetainablePacket)

	currentSeq := currentPacket.Header().SequenceNumber

	latency := time.Since(currentPacket.AddedTime())

	if p.oldestPacket != nil && time.Since(p.oldestPacket.AddedTime()) > maxLatency {
		// we have waited too long, we should send the packets
		return p.pop(e)
	}

	if !p.init && latency > minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*rtppool.RetainablePacket).Header().SequenceNumber, currentSeq) {
		// first packet to send, but make sure we have the packet in order
		p.initSequence = currentSeq
		p.init = true
		return p.pop(e)
	}

	if (p.lastSequenceNumber < currentSeq || p.lastSequenceNumber-currentSeq > uint16SizeHalf) && currentSeq-p.lastSequenceNumber == 1 {
		// the current packet is in sequence with the last packet we popped
		if time.Since(currentPacket.AddedTime()) > minLatency {
			// passed the min latency
			return p.pop(e)
		}
	}

	// there is a gap between the last packet we popped and the current packet
	// we should wait for the next packet

	// but check with the latency if there is a packet pass the max latency
	packetLatency := time.Since(currentPacket.AddedTime())
	// glog.Info("packet latency: ", packetLatency, " gap: ", gap, " currentSeq: ", currentSeq, " nextSeq: ", nextSeq)
	if packetLatency > maxLatency {
		// we have waited too long, we should send the packets
		glog.Warning("packet cache: packet sequence ", currentPacket.Header().SequenceNumber, " latency ", packetLatency, ", reached max latency ", maxLatency, ", will sending the packets")
		return p.pop(e)
	}

	return nil
}

func (p *packetBuffers) Pop() *rtppool.RetainablePacket {
	p.mu.RLock()
	frontElement := p.buffers.Front()
	p.mu.RUnlock()
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

func (p *packetBuffers) checkPacketWaitTime() {
	if p.buffers.Len() == 0 {
		return
	}

	for e := p.buffers.Front(); e != nil && e.Next() != nil; e = e.Next() {

		currentPacket := e.Value.(*rtppool.RetainablePacket)
		currentSeq := currentPacket.Header().SequenceNumber

		nextPacket := e.Next().Value.(*rtppool.RetainablePacket)
		nextSeq := nextPacket.Header().SequenceNumber

		if (currentSeq < nextSeq || currentSeq-nextSeq > uint16SizeHalf) && nextSeq-currentSeq == 1 {
			p.recordWaitTime(e)
		} else {
			break
		}
	}
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

		// calculate the 75th percentile of the wait times
		// sort the wait times

		sortedWaitTimes := make([]time.Duration, len(p.waitTimes))
		copy(sortedWaitTimes, p.waitTimes)

		sort.Slice(sortedWaitTimes, func(i, j int) bool {
			return sortedWaitTimes[i] < sortedWaitTimes[j]
		})

		// get the 75th percentile
		percentile90thIndex := int(float64(len(sortedWaitTimes)) * 0.90)
		percentile75thIndex := int(float64(len(sortedWaitTimes)) * 0.75)
		percentile60thIndex := int(float64(len(sortedWaitTimes)) * 0.60)
		percentile90th := sortedWaitTimes[percentile90thIndex]
		percentile75th := sortedWaitTimes[percentile75thIndex]
		percentile60th := sortedWaitTimes[percentile60thIndex]

		glog.Info("packet cache: 90th wait time: ", percentile90th, " 75th wait time ", percentile75th, " 60th wait time ", percentile60th)

		if percentile75th > p.minLatency {
			// increase the min latency
			glog.Info("packet cache: 75th wait time ", percentile75th, ", increasing min latency from ", p.minLatency)
			p.latencyMu.Lock()
			p.minLatency = percentile75th
			p.latencyMu.Unlock()
		} else if percentile75th < p.minLatency {
			// decrease the min latency
			glog.Info("packet cache: 75th wait time ", percentile75th, ", decreasing min latency from ", p.minLatency)
			p.latencyMu.Lock()
			p.minLatency = percentile75th
			p.latencyMu.Unlock()
		}

		p.waitTimeResetCounter = 0
	} else {
		p.waitTimeResetCounter++
	}
}
