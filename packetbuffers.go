package sfu

import (
	"container/list"
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/logging"
)

var (
	ErrPacketTooLate   = errors.New("packetbuffers: packet is too late")
	ErrPacketDuplicate = errors.New("packetbuffers: packet is duplicate")
)

type packet struct {
	packet    *rtppool.RetainablePacket
	addedTime time.Time
}

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
	oldestPacket         *packet
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
		p.buffers.PushFront(&packet{
			packet:    pkt,
			addedTime: time.Now(),
		})

		return ErrPacketTooLate
	}

	if p.buffers.Len() == 0 {
		p.buffers.PushBack(&packet{
			packet:    pkt,
			addedTime: time.Now(),
		})

		return nil
	}
	// add packet in order
Loop:
	for e := p.buffers.Back(); e != nil; e = e.Prev() {
		currentPkt := e.Value.(*packet)

		if err := currentPkt.packet.Retain(); err != nil {
			// already released
			continue
		}

		if currentPkt.packet.Header().SequenceNumber == pkt.Header().SequenceNumber {
			// p.log.Warnf("packet cache: packet sequence ", pkt.SequenceNumber, " already exists in the cache, will not adding the packet")
			currentPkt.packet.Release()
			return ErrPacketDuplicate
		}

		if currentPkt.packet.Header().SequenceNumber < pkt.Header().SequenceNumber && pkt.Header().SequenceNumber-currentPkt.packet.Header().SequenceNumber < uint16SizeHalf {
			p.buffers.InsertAfter(&packet{
				packet:    pkt,
				addedTime: time.Now(),
			}, e)
			currentPkt.packet.Release()
			break Loop
		}

		if currentPkt.packet.Header().SequenceNumber-pkt.Header().SequenceNumber > uint16SizeHalf {
			p.buffers.InsertAfter(&packet{
				packet:    pkt,
				addedTime: time.Now(),
			}, e)
			currentPkt.packet.Release()
			break Loop
		}

		if e.Prev() == nil {
			p.buffers.PushFront(&packet{
				packet:    pkt,
				addedTime: time.Now(),
			})
			currentPkt.packet.Release()
			break Loop
		}

		currentPkt.packet.Release()
	}

	return nil
}

func (p *packetBuffers) pop(el *list.Element) *packet {
	pkt := el.Value.(*packet)

	if err := pkt.packet.Retain(); err != nil {
		// already released
		return nil
	}

	defer func() {
		pkt.packet.Release()
	}()

	// make sure packet is not late
	if IsRTPPacketLate(pkt.packet.Header().SequenceNumber, p.lastSequenceNumber) {
		p.log.Warnf("packet cache: packet sequence ", pkt.packet.Header().SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber)
	}

	if p.init && pkt.packet.Header().SequenceNumber > p.lastSequenceNumber && pkt.packet.Header().SequenceNumber-p.lastSequenceNumber > 1 {
		// make sure packet has no gap
		p.log.Warnf("packet cache: packet sequence ", pkt.packet.Header().SequenceNumber, " has a gap with last sent ", p.lastSequenceNumber)
	}

	p.mu.Lock()
	p.lastSequenceNumber = pkt.packet.Header().SequenceNumber
	p.packetCount++
	p.mu.Unlock()

	if p.oldestPacket != nil && p.oldestPacket.packet.Header().SequenceNumber == pkt.packet.Header().SequenceNumber {
		// oldest packet will be remove, find the next oldest packet in the list
		p.mu.RLock()
		for e := el.Next(); e != nil; e = e.Next() {
			packet := e.Value.(*packet)
			if packet.addedTime.After(p.oldestPacket.addedTime) {
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

func (p *packetBuffers) flush() []*packet {
	packets := make([]*packet, 0)

	if p.oldestPacket != nil && time.Since(p.oldestPacket.addedTime) > p.maxLatency {
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

func (p *packetBuffers) sendOldestPacket() *packet {
	for e := p.buffers.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*packet)
		if packet.packet.Header().SequenceNumber == p.oldestPacket.packet.Header().SequenceNumber {
			return p.pop(e)
		}
	}

	return nil
}

func (p *packetBuffers) fetch(e *list.Element) *packet {
	p.latencyMu.RLock()
	maxLatency := p.maxLatency
	minLatency := p.minLatency
	p.latencyMu.RUnlock()

	currentPacket := e.Value.(*packet)

	currentSeq := currentPacket.packet.Header().SequenceNumber

	latency := time.Since(currentPacket.addedTime)

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

		if time.Since(currentPacket.addedTime) > minLatency {
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
		p.log.Warnf("packet cache: packet sequence ", currentPacket.packet.Header().SequenceNumber, " latency ", latency, ", reached max latency ", maxLatency, ", will sending the packets")
		return p.pop(e)
	}

	return nil
}

func (p *packetBuffers) Pop() *packet {
	p.mu.RLock()
	if p.oldestPacket != nil && time.Since(p.oldestPacket.addedTime) > p.maxLatency {
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

func (p *packetBuffers) Flush() []*packet {
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
		pkt := e.Value.(*packet)

		// make sure call retain to prevent packet from being released when we are still using it
		if err := pkt.packet.Retain(); err != nil {
			// already released
			continue
		}

		currentSeq := pkt.packet.Header().SequenceNumber
		latency := time.Since(pkt.addedTime)

		if !p.init && latency > p.minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*rtppool.RetainablePacket).Header().SequenceNumber, currentSeq) {
			// signal first packet to send
			p.packetAvailableWait.Signal()
		} else if (p.lastSequenceNumber < currentSeq || p.lastSequenceNumber-currentSeq > uint16SizeHalf) && currentSeq-p.lastSequenceNumber == 1 {
			// the current packet is in sequence with the last packet we popped
			p.recordWaitTime(e)

			if time.Since(pkt.addedTime) > p.minLatency {
				// passed the min latency
				p.packetAvailableWait.Signal()
			}
		} else if latency > p.maxLatency {
			// passed the max latency
			p.packetAvailableWait.Signal()
		}

		// release the packet after we are done with it
		pkt.packet.Release()
	}
}

func (p *packetBuffers) recordWaitTime(el *list.Element) {
	p.waitTimeMu.Lock()
	defer p.waitTimeMu.Unlock()

	pkt := el.Value.(*packet)

	if p.lastSequenceWaitTime == pkt.packet.Header().SequenceNumber || IsRTPPacketLate(pkt.packet.Header().SequenceNumber, p.lastSequenceWaitTime) || p.previousAddedTime.IsZero() {
		// don't record late packet or already recorded packet
		if p.previousAddedTime.IsZero() {
			p.previousAddedTime = pkt.addedTime
		}

		return
	}

	p.lastSequenceWaitTime = pkt.packet.Header().SequenceNumber

	// remove oldest packet from the wait times if more than 500
	if uint16(len(p.waitTimes)+1) > waitTimeSize {
		p.waitTimes = p.waitTimes[1:]
	}

	gapTime := pkt.addedTime.Sub(p.previousAddedTime)

	p.previousAddedTime = pkt.addedTime
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
