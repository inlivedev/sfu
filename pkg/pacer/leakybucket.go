// modified version of pion interceptor leaky_bucket_pacer.go
// https://github.com/pion/interceptor/blob/master/pkg/gcc/leaky_bucket_pacer.go
package pacer

import (
	"container/list"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
)

var (
	ErrNoQueueFound   = errors.New("no queue found for ssrc")
	ErrDuplicate      = errors.New("packet sequence already exists in the queue")
	ErrPacketRejected = errors.New("pacer rejected packet")
)

const (
	uint16SizeHalf = uint16(1 << 15)

	// defaultMTU is the minimum burst size in bytes, so that a full-size packet
	// can always be sent right after the pacer has been idle.
	defaultMTU = 1500
)

// resendTracker matches the exported methods of pion/interceptor's
// internal rtpbuffer.Resend (interceptor >= v0.1.51). The NACK responder puts
// one in the packet attributes so it can suppress a retransmission while the
// previous one is still queued in the pacer. The key type is internal, so we
// find the value by its method set instead of by key.
type resendTracker interface {
	Retain()
	Release(sent bool)
}

func findResendTracker(attributes interceptor.Attributes) resendTracker {
	for _, v := range attributes {
		if r, ok := v.(resendTracker); ok {
			return r
		}
	}

	return nil
}

type item struct {
	packet     *rtppool.RetainablePacket
	size       int
	attributes interceptor.Attributes
	resend     resendTracker
}

// release returns the packet to the pool and reports the outcome to the NACK
// responder, if it is tracking this packet.
func (i *item) release(sent bool) {
	if i.resend != nil {
		i.resend.Release(sent)
	}
	i.packet.Release()
}

type queue struct {
	list.List
	mu sync.RWMutex
}

func (q *queue) popFront() *item {
	q.mu.Lock()
	defer q.mu.Unlock()

	for e := q.Front(); e != nil; e = q.Front() {
		if i, ok := q.List.Remove(e).(*item); ok {
			return i
		}
	}

	return nil
}

// LeakyBucketPacer implements a leaky bucket pacing algorithm
type LeakyBucketPacer struct {
	allowDuplicate    bool
	f                 float64
	targetBitrate     int
	targetBitrateLock sync.Mutex

	pacingInterval time.Duration

	qLock  sync.RWMutex
	queues map[uint32]*queue
	// order lists the queues' SSRCs so drain can round-robin fairly;
	// nextQueue is the position to resume from on the next tick.
	order     []uint32
	nextQueue int
	done      chan struct{}
	closed    bool

	ssrcToWriter map[uint32]interceptor.RTPWriter
	writerLock   sync.RWMutex
	log          logging.LeveledLogger
	rtppool      *rtppool.RTPPool
}

// NewLeakyBucketPacer initializes a new LeakyBucketPacer
func NewLeakyBucketPacer(log logging.LeveledLogger, initialBitrate int, allowDuplicate bool) *LeakyBucketPacer {
	p := &LeakyBucketPacer{
		allowDuplicate: allowDuplicate,
		f:              1.5,
		targetBitrate:  initialBitrate,
		pacingInterval: 5 * time.Millisecond,
		qLock:          sync.RWMutex{},
		done:           make(chan struct{}),
		ssrcToWriter:   map[uint32]interceptor.RTPWriter{},
		queues:         map[uint32]*queue{},
		log:            log,
		rtppool:        rtppool.New(),
	}

	go p.Run()
	return p
}

// AddStream adds a new stream and its corresponding writer to the pacer
func (p *LeakyBucketPacer) AddStream(ssrc uint32, writer interceptor.RTPWriter) {
	p.writerLock.Lock()
	p.ssrcToWriter[ssrc] = writer
	p.writerLock.Unlock()

	p.qLock.Lock()
	if _, ok := p.queues[ssrc]; !ok {
		p.order = append(p.order, ssrc)
	}
	p.queues[ssrc] = &queue{
		List: list.List{},
		mu:   sync.RWMutex{},
	}
	p.qLock.Unlock()
}

// SetTargetBitrate updates the target bitrate at which the pacer is allowed to
// send packets. The pacer may exceed this limit by p.f
func (p *LeakyBucketPacer) SetTargetBitrate(rate int) {
	p.targetBitrateLock.Lock()
	defer p.targetBitrateLock.Unlock()
	p.targetBitrate = int(p.f * float64(rate))
}

func (p *LeakyBucketPacer) getTargetBitrate() int {
	p.targetBitrateLock.Lock()
	defer p.targetBitrateLock.Unlock()

	return p.targetBitrate
}

func (p *LeakyBucketPacer) getWriter(ssrc uint32) interceptor.RTPWriter {
	p.writerLock.RLock()
	defer p.writerLock.RUnlock()

	writer, ok := p.ssrcToWriter[ssrc]

	if !ok {
		p.log.Warnf("no writer found for ssrc: %v", ssrc)
		return nil
	}

	return writer
}

func (p *LeakyBucketPacer) getQueue(ssrc uint32) *queue {
	p.qLock.RLock()
	defer p.qLock.RUnlock()
	queue, ok := p.queues[ssrc]
	if !ok {
		return nil
	}
	return queue

}

// Write queues a packet with header and payload for a previously registered
// stream. Packets are kept in sequence number order per stream.
func (p *LeakyBucketPacer) Write(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
	queue := p.getQueue(header.SSRC)
	if queue == nil {
		p.log.Errorf("no queue found for ssrc: %v", header.SSRC)

		return 0, ErrNoQueueFound
	}

	// NewPacket fails for payloads larger than the pool buffer. The payload
	// comes from the remote publisher, so this must be an error, not a panic.
	pkt, err := p.rtppool.PacketManager.NewPacket(header, payload, attributes)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPacketRejected, err)
	}

	newItem := &item{
		packet:     pkt,
		size:       header.MarshalSize() + len(payload),
		attributes: maps.Clone(attributes),
		resend:     findResendTracker(attributes),
	}
	if newItem.resend != nil {
		newItem.resend.Retain()
	}

	queue.mu.Lock()
	inserted := p.insert(queue, newItem)
	queue.mu.Unlock()

	if !inserted {
		newItem.release(false)
	}

	return newItem.size, nil
}

// insert places newItem in sequence number order. It returns false when the
// item was dropped as a duplicate. The caller must hold queue.mu.
func (p *LeakyBucketPacer) insert(queue *queue, newItem *item) bool {
	if queue.Len() == 0 {
		queue.PushBack(newItem)

		return true
	}

	seq := newItem.packet.Header().SequenceNumber

	for e := queue.Back(); e != nil; e = e.Prev() {
		currentCache, _ := e.Value.(*item)
		currentSeq := currentCache.packet.Header().SequenceNumber

		if currentSeq == seq {
			if p.allowDuplicate {
				queue.InsertAfter(newItem, e)

				return true
			}

			return false
		}

		if currentSeq < seq && seq-currentSeq < uint16SizeHalf {
			queue.InsertAfter(newItem, e)

			return true
		}

		if currentSeq-seq > uint16SizeHalf {
			queue.InsertAfter(newItem, e)

			return true
		}
	}

	queue.PushFront(newItem)

	return true
}

// burstBytes is the most budget the pacer may bank while it has nothing to
// send: one pacing interval at the target rate, but never less than one MTU.
func (p *LeakyBucketPacer) burstBytes(bitrate int) int {
	return max(defaultMTU, int(float64(bitrate)*p.pacingInterval.Seconds()/8))
}

// Run starts the LeakyBucketPacer
func (p *LeakyBucketPacer) Run() {
	ticker := time.NewTicker(p.pacingInterval)
	defer ticker.Stop()

	// budget is in bytes and is carried between ticks. It can go negative
	// when a packet is larger than the remaining budget; that debt is paid
	// off on later ticks, so oversized packets never stall the queue.
	budget := 0
	lastTick := time.Now()

	for {
		select {
		case <-p.done:
			return
		case now := <-ticker.C:
			bitrate := p.getTargetBitrate()
			budget += int(now.Sub(lastTick).Seconds() * float64(bitrate) / 8)
			lastTick = now

			budget = p.drain(budget)

			// Don't bank unlimited budget while idle, or the next frame
			// would be sent as one large burst.
			budget = min(budget, p.burstBytes(bitrate))
		}
	}
}

// drain sends packets round-robin across the stream queues until the budget
// is spent or every queue is empty, and returns the remaining budget. The
// round-robin position carries over between ticks, so when only a packet or
// two fits per tick the streams still take turns. Only Run calls drain.
func (p *LeakyBucketPacer) drain(budget int) int {
	p.qLock.RLock()
	defer p.qLock.RUnlock()

	idle := 0
	for budget > 0 && idle < len(p.order) {
		if p.nextQueue >= len(p.order) {
			p.nextQueue = 0
		}
		queue := p.queues[p.order[p.nextQueue]]
		p.nextQueue++

		next := queue.popFront()
		if next == nil {
			idle++

			continue
		}

		idle = 0
		budget -= p.send(next)
	}

	return budget
}

// send writes one queued item and returns the number of bytes it costs
// against the budget.
func (p *LeakyBucketPacer) send(next *item) int {
	writer := p.getWriter(next.packet.Header().SSRC)
	if writer == nil {
		next.release(false)

		return 0
	}

	n, err := writer.Write(next.packet.Header(), next.packet.Payload(), next.attributes)
	if err != nil {
		p.log.Errorf("failed to write packet: %v", err)
		// still charge the packet so a failing writer can't drain the
		// whole queue in a single tick
		n = next.size
	}

	next.release(err == nil)

	return n
}

// Close flushes the queued packets and stops the LeakyBucketPacer
func (p *LeakyBucketPacer) Close() error {
	p.qLock.Lock()
	defer p.qLock.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true

	for ssrc, queue := range p.queues {
		writer := p.getWriter(ssrc)

		queue.mu.Lock()
		for e := queue.Front(); e != nil; e = e.Next() {
			i, ok := e.Value.(*item)
			if !ok {
				continue
			}

			sent := false
			if writer != nil {
				_, err := writer.Write(i.packet.Header(), i.packet.Payload(), i.attributes)
				sent = err == nil
			}

			i.release(sent)
		}
		queue.Init()
		queue.mu.Unlock()

		delete(p.queues, ssrc)
	}
	p.order = nil

	close(p.done)

	return nil
}
