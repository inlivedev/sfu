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

type Packet struct {
	Packet    *rtppool.RetainablePacket
	addedTime time.Time
}

// buffer ring for cached packets
type PacketBuffers struct {
	context            context.Context
	init               bool
	mu                 sync.RWMutex
	buffers            *list.List
	lastSequenceNumber uint16
	latencyMu          sync.RWMutex
	// min duration to wait before sending
	minLatency time.Duration
	// max duration to wait before sending
	maxLatency           time.Duration
	oldestPacket         *Packet
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

func NewPacketBuffers(ctx context.Context, minLatency, maxLatency time.Duration, dynamicLatency bool, log logging.LeveledLogger) *PacketBuffers {
	ctx, cancel := context.WithCancel(ctx)
	p := &PacketBuffers{
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

func (p *PacketBuffers) MaxLatency() time.Duration {
	p.latencyMu.RLock()
	defer p.latencyMu.RUnlock()
	return p.maxLatency
}

func (p *PacketBuffers) MinLatency() time.Duration {
	p.latencyMu.RLock()
	defer p.latencyMu.RUnlock()
	return p.minLatency
}
func (p *PacketBuffers) Initiated() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.init
}

func (p *PacketBuffers) Add(pkt *rtppool.RetainablePacket) error {
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
		p.buffers.PushFront(&Packet{
			Packet:    pkt,
			addedTime: time.Now(),
		})

		return ErrPacketTooLate
	}

	if p.buffers.Len() == 0 {
		p.buffers.PushBack(&Packet{
			Packet:    pkt,
			addedTime: time.Now(),
		})

		return nil
	}
	// add packet in order
Loop:
	for e := p.buffers.Back(); e != nil; e = e.Prev() {
		currentpkt := e.Value.(*Packet)

		if err := currentpkt.Packet.Retain(); err != nil {
			// already released
			continue
		}

		if currentpkt.Packet.Header().SequenceNumber == pkt.Header().SequenceNumber {
			// p.log.Warnf("packet cache: packet sequence ", pkt.SequenceNumber, " already exists in the cache, will not adding the packet")
			currentpkt.Packet.Release()
			return ErrPacketDuplicate
		}

		if currentpkt.Packet.Header().SequenceNumber < pkt.Header().SequenceNumber && pkt.Header().SequenceNumber-currentpkt.Packet.Header().SequenceNumber < uint16SizeHalf {
			p.buffers.InsertAfter(&Packet{
				Packet:    pkt,
				addedTime: time.Now(),
			}, e)
			currentpkt.Packet.Release()
			break Loop
		}

		if currentpkt.Packet.Header().SequenceNumber-pkt.Header().SequenceNumber > uint16SizeHalf {
			p.buffers.InsertAfter(&Packet{
				Packet:    pkt,
				addedTime: time.Now(),
			}, e)
			currentpkt.Packet.Release()
			break Loop
		}

		if e.Prev() == nil {
			p.buffers.PushFront(&Packet{
				Packet:    pkt,
				addedTime: time.Now(),
			})
			currentpkt.Packet.Release()
			break Loop
		}

		currentpkt.Packet.Release()
	}

	return nil
}

func (p *PacketBuffers) pop(el *list.Element) *Packet {
	pkt := el.Value.(*Packet)

	if err := pkt.Packet.Retain(); err != nil {
		// already released
		return nil
	}

	defer func() {
		pkt.Packet.Release()
	}()

	// make sure packet is not late
	if IsRTPPacketLate(pkt.Packet.Header().SequenceNumber, p.lastSequenceNumber) {
		p.log.Warnf("packet cache: packet sequence ", pkt.Packet.Header().SequenceNumber, " is too late, last sent was ", p.lastSequenceNumber)
	}

	if p.init && pkt.Packet.Header().SequenceNumber > p.lastSequenceNumber && pkt.Packet.Header().SequenceNumber-p.lastSequenceNumber > 1 {
		// make sure packet has no gap
		p.log.Warnf("packet cache: packet sequence ", pkt.Packet.Header().SequenceNumber, " has a gap with last sent ", p.lastSequenceNumber)
	}

	p.mu.Lock()
	p.lastSequenceNumber = pkt.Packet.Header().SequenceNumber
	p.packetCount++
	p.mu.Unlock()

	if p.oldestPacket != nil && p.oldestPacket.Packet.Header().SequenceNumber == pkt.Packet.Header().SequenceNumber {
		// oldest packet will be remove, find the next oldest packet in the list
		p.mu.RLock()
		for e := el.Next(); e != nil; e = e.Next() {
			packet := e.Value.(*Packet)
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

func (p *PacketBuffers) flush() []*Packet {
	packets := make([]*Packet, 0)

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

func (p *PacketBuffers) sendOldestPacket() *Packet {
	for e := p.buffers.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*Packet)
		if packet.Packet.Header().SequenceNumber == p.oldestPacket.Packet.Header().SequenceNumber {
			return p.pop(e)
		}
	}

	return nil
}

func (p *PacketBuffers) fetch(e *list.Element) *Packet {
	p.latencyMu.RLock()
	maxLatency := p.maxLatency
	minLatency := p.minLatency
	p.latencyMu.RUnlock()

	currentPacket := e.Value.(*Packet)

	currentSeq := currentPacket.Packet.Header().SequenceNumber

	latency := time.Since(currentPacket.addedTime)

	if !p.Initiated() && latency > minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*Packet).Packet.Header().SequenceNumber, currentSeq) {
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
		p.log.Warnf("packet cache: packet sequence ", currentPacket.Packet.Header().SequenceNumber, " latency ", latency, ", reached max latency ", maxLatency, ", will sending the packets")
		return p.pop(e)
	}

	return nil
}

func (p *PacketBuffers) Pop() *Packet {
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

	item := frontElement.Value.(*Packet)
	if err := item.Packet.Retain(); err != nil {
		return nil
	}

	defer func() {
		item.Packet.Release()
	}()

	if IsRTPPacketLate(item.Packet.Header().SequenceNumber, p.lastSequenceNumber) {

		return p.pop(frontElement)
	}

	return p.fetch(frontElement)
}

func (p *PacketBuffers) Flush() []*Packet {
	return p.flush()
}

func (p *PacketBuffers) Last() *Packet {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.buffers.Len() == 0 {
		return nil
	}

	return p.buffers.Back().Value.(*Packet)
}

func (p *PacketBuffers) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.buffers.Len()
}

func (p *PacketBuffers) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for e := p.buffers.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*Packet)
		packet.Packet.Release()
		p.buffers.Remove(e)
	}

	p.buffers.Init()

	p.oldestPacket = nil
}

func (p *PacketBuffers) Close() {
	p.mu.Lock()
	p.ended = true
	p.mu.Unlock()

	p.Clear()

	// make sure we don't have any waiters
	p.packetAvailableWait.Signal()
}
func (p *PacketBuffers) WaitAvailablePacket() {
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

func (p *PacketBuffers) checkOrderedPacketAndRecordTimes() {
	for e := p.buffers.Front(); e != nil; e = e.Next() {
		pkt := e.Value.(*Packet)

		// make sure call retain to prevent packet from being released when we are still using it
		if err := pkt.Packet.Retain(); err != nil {
			// already released
			continue
		}

		currentSeq := pkt.Packet.Header().SequenceNumber
		latency := time.Since(pkt.addedTime)

		if !p.init && latency > p.minLatency && e.Next() != nil && !IsRTPPacketLate(e.Next().Value.(*Packet).Packet.Header().SequenceNumber, currentSeq) {
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
		pkt.Packet.Release()
	}
}

func (p *PacketBuffers) recordWaitTime(el *list.Element) {
	p.waitTimeMu.Lock()
	defer p.waitTimeMu.Unlock()

	pkt := el.Value.(*Packet)

	if p.lastSequenceWaitTime == pkt.Packet.Header().SequenceNumber || IsRTPPacketLate(pkt.Packet.Header().SequenceNumber, p.lastSequenceWaitTime) || p.previousAddedTime.IsZero() {
		// don't record late packet or already recorded packet
		if p.previousAddedTime.IsZero() {
			p.previousAddedTime = pkt.addedTime
		}

		return
	}

	p.lastSequenceWaitTime = pkt.Packet.Header().SequenceNumber

	// remove oldest packet from the wait times if more than 500
	if uint16(len(p.waitTimes)+1) > waitTimeSize {
		p.waitTimes = p.waitTimes[1:]
	}

	gapTime := pkt.addedTime.Sub(p.previousAddedTime)

	p.previousAddedTime = pkt.addedTime
	p.waitTimes = append(p.waitTimes, gapTime)

}

func (p *PacketBuffers) checkWaitTimeAdjuster() {
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
