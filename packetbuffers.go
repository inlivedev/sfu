package sfu

import (
	"container/list"
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/samespace/sfu/pkg/rtppool"
)

var (
	ErrPacketTooLate   = errors.New("packetbuffers: packet is too late")
	ErrPacketDuplicate = errors.New("packetbuffers: packet is duplicate")
)

// buffer ring for cached packets
type packetBuffers struct {
	context            context.Context
	cancel             context.CancelFunc
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
	previousAddedTime    time.Time
	lastSequenceWaitTime uint16
	waitTimeResetCounter uint16
	packetAvailableWait  *sync.Cond
	enableDynamicLatency bool
	ended                bool
	log                  logging.LeveledLogger
}

const waitTimeSize = 2500

func newPacketBuffers(ctx context.Context, minLatency, maxLatency time.Duration, dynamicLatency bool, log logging.LeveledLogger) *packetBuffers {
	ctx, cancel := context.WithCancel(ctx)
	p := &packetBuffers{
		context:              ctx,
		mu:                   sync.RWMutex{},
		buffers:              list.New(),
		latencyMu:            sync.RWMutex{},
		minLatency:           minLatency,
		maxLatency:           maxLatency,
		waitTimeMu:           sync.RWMutex{},
		waitTimes:            make([]time.Duration, waitTimeSize),
		lastSequenceWaitTime: 0,
		packetAvailableWait:  sync.NewCond(&sync.Mutex{}),
		enableDynamicLatency: dynamicLatency,
		log:                  log,
	}

	go func() {
		defer p.Close()
		defer cancel()
		<-ctx.Done()
	}()

	return p
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
func (p *packetBuffers) Initiated() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.init
}

func (p *packetBuffers) Add(pkt *rtppool.RetainablePacket) error {
	p.mu.Lock()
	defer func() {
		p.checkOrderedPacketAndRecordTimes()
		if p.enableDynamicLatency {
			p.checkWaitTimeAdjuster()
		}

		p.mu.Unlock()
	}()

	if p.init &&
		(p.lastSequenceNumber == pkt.Header().SequenceNumber || IsRTPPacketLate(pkt.Header().SequenceNumber, p.lastSequenceNumber)) {
		p.log.Warnf("packet cache: packet sequence ", pkt.Header().SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber, ", will not adding the packet")

		// add to to front of the list to make sure pop take it first
		p.buffers.PushFront(pkt)

		return ErrPacketTooLate
	}

	if p.buffers.Len() == 0 {
		p.buffers.PushBack(pkt)

		return nil
	}
	// add packet in order
Loop:
	for e := p.buffers.Back(); e != nil; e = e.Prev() {
		currentPkt := e.Value.(*rtppool.RetainablePacket)

		if err := currentPkt.Retain(); err != nil {
			// already released
			continue
		}

		if currentPkt.Header().SequenceNumber == pkt.Header().SequenceNumber {
			// p.log.Warnf("packet cache: packet sequence ", pkt.SequenceNumber, " already exists in the cache, will not adding the packet")
			currentPkt.Release()
			return ErrPacketDuplicate
		}

		if currentPkt.Header().SequenceNumber < pkt.Header().SequenceNumber && pkt.Header().SequenceNumber-currentPkt.Header().SequenceNumber < uint16SizeHalf {
			p.buffers.InsertAfter(pkt, e)
			currentPkt.Release()
			break Loop
		}

		if currentPkt.Header().SequenceNumber-pkt.Header().SequenceNumber > uint16SizeHalf {
			p.buffers.InsertAfter(pkt, e)
			currentPkt.Release()
			break Loop
		}

		if e.Prev() == nil {
			p.buffers.PushFront(pkt)
			currentPkt.Release()
			break Loop
		}

		currentPkt.Release()
	}

	return nil
}

func (p *packetBuffers) pop(el *list.Element) *rtppool.RetainablePacket {
	pkt := el.Value.(*rtppool.RetainablePacket)

	if err := pkt.Retain(); err != nil {
		// already released
		return nil
	}

	defer func() {
		pkt.Release()
	}()

	// make sure packet is not late
	if IsRTPPacketLate(pkt.Header().SequenceNumber, p.lastSequenceNumber) {
		p.log.Warnf("packet cache: packet sequence ", pkt.Header().SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber)
	}

	if p.init && pkt.Header().SequenceNumber > p.lastSequenceNumber && pkt.Header().SequenceNumber-p.lastSequenceNumber > 1 {
		// make sure packet has no gap
		p.log.Warnf("packet cache: packet sequence ", pkt.Header().SequenceNumber, " has a gap with last sent ", p.lastSequenceNumber)
	}

	p.mu.Lock()
	p.lastSequenceNumber = pkt.Header().SequenceNumber
	p.packetCount++
	p.mu.Unlock()

	if p.oldestPacket != nil && p.oldestPacket.Header().SequenceNumber == pkt.Header().SequenceNumber {
		// oldest packet will be remove, find the next oldest packet in the list
		p.mu.RLock()
		for e := el.Next(); e != nil; e = e.Next() {
			packet := e.Value.(*rtppool.RetainablePacket)
			if packet.AddedTime().After(p.oldestPacket.AddedTime()) {
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
	packets := make([]*rtppool.RetainablePacket, 0)

	if p.oldestPacket != nil && time.Since(p.oldestPacket.AddedTime()) > p.maxLatency {
		// we have waited too long, we should send the packets

		packets = append(packets, p.sendOldestPacket())
	}
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

func (p *packetBuffers) sendOldestPacket() *rtppool.RetainablePacket {
	for e := p.buffers.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*rtppool.RetainablePacket)
		if packet.Header().SequenceNumber == p.oldestPacket.Header().SequenceNumber {
			return p.pop(e)
		}
	}

	return nil
}

func (p *packetBuffers) fetch(e *list.Element) *rtppool.RetainablePacket {
	p.latencyMu.RLock()
	maxLatency := p.maxLatency
	minLatency := p.minLatency
	p.latencyMu.RUnlock()

	currentPacket := e.Value.(*rtppool.RetainablePacket)

	currentSeq := currentPacket.Header().SequenceNumber

	latency := time.Since(currentPacket.AddedTime())

	if !p.Initiated() && latency > minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*rtppool.RetainablePacket).Header().SequenceNumber, currentSeq) {
		// first packet to send, but make sure we have the packet in order
		p.mu.Lock()
		p.initSequence = currentSeq
		p.init = true
		p.mu.Unlock()

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

	// p.log.Infof("packet latency: ", packetLatency, " gap: ", gap, " currentSeq: ", currentSeq, " nextSeq: ", nextSeq)
	if latency > maxLatency {
		// we have waited too long, we should send the packets
		p.log.Warnf("packet cache: packet sequence ", currentPacket.Header().SequenceNumber, " latency ", latency, ", reached max latency ", maxLatency, ", will sending the packets")
		return p.pop(e)
	}

	return nil
}

func (p *packetBuffers) Pop() *rtppool.RetainablePacket {
	p.mu.RLock()
	if p.oldestPacket != nil && time.Since(p.oldestPacket.AddedTime()) > p.maxLatency {
		p.mu.RUnlock()
		// we have waited too long, we should send the packets
		return p.sendOldestPacket()
	}

	frontElement := p.buffers.Front()
	p.mu.RUnlock()
	if frontElement == nil {

		return nil
	}

	item := frontElement.Value.(*rtppool.RetainablePacket)
	if err := item.Retain(); err != nil {
		return nil
	}

	defer func() {
		item.Release()
	}()

	if IsRTPPacketLate(item.Header().SequenceNumber, p.lastSequenceNumber) {

		return p.pop(frontElement)
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

func (p *packetBuffers) Close() {
	p.mu.Lock()
	p.ended = true
	p.mu.Unlock()

	p.Clear()

	// make sure we don't have any waiters
	p.packetAvailableWait.Signal()
}
func (p *packetBuffers) WaitAvailablePacket() {
	p.mu.RLock()
	if p.buffers.Len() == 0 {
		p.mu.RUnlock()
		return
	}

	if p.ended {
		return
	}

	p.mu.RUnlock()

	p.packetAvailableWait.L.Lock()
	defer p.packetAvailableWait.L.Unlock()

	p.packetAvailableWait.Wait()
}

func (p *packetBuffers) checkOrderedPacketAndRecordTimes() {
	for e := p.buffers.Front(); e != nil; e = e.Next() {
		pkt := e.Value.(*rtppool.RetainablePacket)

		// make sure call retain to prevent packet from being released when we are still using it
		if err := pkt.Retain(); err != nil {
			// already released
			continue
		}

		currentSeq := pkt.Header().SequenceNumber
		latency := time.Since(pkt.AddedTime())

		if !p.init && latency > p.minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*rtppool.RetainablePacket).Header().SequenceNumber, currentSeq) {
			// signal first packet to send
			p.packetAvailableWait.Signal()
		} else if (p.lastSequenceNumber < currentSeq || p.lastSequenceNumber-currentSeq > uint16SizeHalf) && currentSeq-p.lastSequenceNumber == 1 {
			// the current packet is in sequence with the last packet we popped
			p.recordWaitTime(e)

			if time.Since(pkt.AddedTime()) > p.minLatency {
				// passed the min latency
				p.packetAvailableWait.Signal()
			}
		} else if latency > p.maxLatency {
			// passed the max latency
			p.packetAvailableWait.Signal()
		}

		// release the packet after we are done with it
		pkt.Release()
	}
}

func (p *packetBuffers) recordWaitTime(el *list.Element) {
	p.waitTimeMu.Lock()
	defer p.waitTimeMu.Unlock()

	pkt := el.Value.(*rtppool.RetainablePacket)

	if p.lastSequenceWaitTime == pkt.Header().SequenceNumber || IsRTPPacketLate(pkt.Header().SequenceNumber, p.lastSequenceWaitTime) || p.previousAddedTime.IsZero() {
		// don't record late packet or already recorded packet
		if p.previousAddedTime.IsZero() {
			p.previousAddedTime = pkt.AddedTime()
		}

		return
	}

	p.lastSequenceWaitTime = pkt.Header().SequenceNumber

	// remove oldest packet from the wait times if more than 500
	if uint16(len(p.waitTimes)+1) > waitTimeSize {
		p.waitTimes = p.waitTimes[1:]
	}

	gapTime := pkt.AddedTime().Sub(p.previousAddedTime)

	p.previousAddedTime = pkt.AddedTime()
	p.waitTimes = append(p.waitTimes, gapTime)

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
		// percentile90thIndex := int(float64(len(sortedWaitTimes)) * 0.90)
		percentileIndex := int(float64(len(sortedWaitTimes)) * 0.7)
		// percentile60thIndex := int(float64(len(sortedWaitTimes)) * 0.60)

		percentile := sortedWaitTimes[percentileIndex]

		if percentile > p.minLatency && percentile < p.maxLatency {
			// increase the min latency
			p.log.Infof("packet cache: set min latency ", percentile, ", increasing min latency from ", p.minLatency)
			p.latencyMu.Lock()
			p.minLatency = percentile
			p.latencyMu.Unlock()
		} else if percentile < p.minLatency && percentile > 0 {
			// decrease the min latency
			p.log.Infof("packet cache: set min latency ", percentile, ", decreasing min latency from ", p.minLatency)
			p.latencyMu.Lock()
			p.minLatency = percentile
			p.latencyMu.Unlock()
		}

		p.waitTimeResetCounter = 0
	} else {
		p.waitTimeResetCounter++
	}
}
