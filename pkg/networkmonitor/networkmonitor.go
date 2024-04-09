package networkmonitor

import (
	"container/list"
	"errors"
	"sync"
	"time"
)

type NetworkConditionType int

const (
	uint16SizeHalf = uint16(1 << 15)
	STABLE         = NetworkConditionType(1)
	UNSTABLE       = NetworkConditionType(0)
)

type NetworkMonitor struct {
	buffers                       *list.List
	init                          bool
	isStable                      bool
	mu                            sync.RWMutex
	maxLatency                    time.Duration
	lastSequenceNumber            uint16
	lastUnstableDetected          time.Time
	onNetworkConditionChangedFunc func(NetworkConditionType)
	unstableCount                 int
	// duration from last unstable detected that considered network recovered
	stableDuration time.Duration
}

type packet struct {
	seq       uint16
	addedTime time.Time
}

var (
	ErrPacketTooLate   = errors.New("packetbuffers: packet is too late")
	ErrPacketDuplicate = errors.New("packetbuffers: packet is duplicate")
)

func New(maxLatency time.Duration, stableDuration time.Duration) *NetworkMonitor {
	return &NetworkMonitor{
		buffers:        list.New(),
		maxLatency:     maxLatency,
		stableDuration: stableDuration,
		isStable:       true,
	}
}

func Default() *NetworkMonitor {
	return New(time.Millisecond*100, time.Second*30)
}

func (n *NetworkMonitor) Add(seq uint16) error {
	n.mu.Lock()
	defer func() {
		n.checkOrderedPacketAndRelease()
		n.mu.Unlock()
	}()

	pkt := packet{
		seq:       seq,
		addedTime: time.Now(),
	}

	if n.buffers.Len() == 0 {
		n.buffers.PushBack(pkt)

		return nil
	}

	// add seq in order
Loop:
	for e := n.buffers.Back(); e != nil; e = e.Prev() {
		currentSeq := e.Value.(packet).seq

		if currentSeq == seq {
			return ErrPacketDuplicate
		}

		if currentSeq < seq && seq-currentSeq < uint16SizeHalf {
			n.buffers.InsertAfter(pkt, e)

			break Loop
		}

		if currentSeq-seq > uint16SizeHalf {
			n.buffers.InsertAfter(pkt, e)

			break Loop
		}

		if e.Prev() == nil {
			n.buffers.PushFront(pkt)

			break Loop
		}
	}

	return nil
}

func (n *NetworkMonitor) checkOrderedPacketAndRelease() {
	for e := n.buffers.Front(); e != nil; e = e.Next() {
		pkt := e.Value.(packet)

		latency := time.Since(pkt.addedTime)
		if !n.init {
			n.init = true
			n.lastSequenceNumber = pkt.seq
			n.buffers.Remove(e)
		} else if (n.lastSequenceNumber < pkt.seq || n.lastSequenceNumber-pkt.seq > uint16SizeHalf) && pkt.seq-n.lastSequenceNumber == 1 {
			// the current packet is in sequence with the last packet we popped
			n.lastSequenceNumber = pkt.seq
			n.buffers.Remove(e)
			if !n.isStable && time.Since(n.lastUnstableDetected) > n.stableDuration {
				n.unstableCount = 0
				n.onStableNetwork()
			}
		} else if latency > n.maxLatency {
			// passed the max latency means this packet is too late and indicate an unstable network
			n.unstableDetected()
			n.buffers.Remove(e)
			n.lastSequenceNumber = pkt.seq
		}
	}
}

func (n *NetworkMonitor) unstableDetected() {
	n.unstableCount++

	n.lastUnstableDetected = time.Now()

	if n.unstableCount > 5 && n.isStable {
		// unstable network detected
		n.onUnstableNetwork()
	}
}

func (n *NetworkMonitor) onUnstableNetwork() {
	if n.onNetworkConditionChangedFunc != nil {
		n.onNetworkConditionChangedFunc(UNSTABLE)
	}

	n.isStable = false
}

func (n *NetworkMonitor) onStableNetwork() {
	if n.onNetworkConditionChangedFunc != nil {
		n.onNetworkConditionChangedFunc(STABLE)
	}

	n.isStable = true
}

func (n *NetworkMonitor) OnNetworkConditionChanged(f func(condition NetworkConditionType)) {
	n.onNetworkConditionChangedFunc = f
}
