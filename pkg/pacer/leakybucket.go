// modified version of pion interceptor leaky_bucket_pacer.go
// https://github.com/pion/interceptor/blob/master/pkg/gcc/leaky_bucket_pacer.go
package pacer

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
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

	// DefaultPacingFactor is how far above the congestion controller's target
	// the pacer may send, the same as libwebrtc's
	// PacingController::kDefaultPaceMultiplier. The SFU forwards whole frames
	// as they arrive from publishers, so the pacer needs room to send a frame
	// burst well inside the frame interval.
	DefaultPacingFactor = 2.5

	// DefaultMaxQueueTime bounds how long a packet may wait in the pacer. When
	// the queue would take longer than this to drain at the target rate, the
	// pacer raises its rate so the queue drains in time, like libwebrtc's
	// PacingController with drain_large_queues (which uses 2 s for a sender
	// that can lower its encoder bitrate). An SFU can only pick layers, and a
	// growing queue makes the delay-based estimator lower its target, which
	// slows the pacer further. A shorter limit breaks that loop sooner.
	DefaultMaxQueueTime = 500 * time.Millisecond
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
	enqueuedAt time.Time
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
	// bytes is the total size of the queued items.
	bytes int
	// audio queues are drained every tick without waiting for budget.
	// It is only changed while holding the pacer's qLock.
	audio bool
}

// popFront removes and returns the first item, if there is one and accept
// returns true for it. A nil accept takes any item.
func (q *queue) popFront(accept func(*item) bool) *item {
	q.mu.Lock()
	defer q.mu.Unlock()

	for e := q.Front(); e != nil; e = q.Front() {
		i, ok := e.Value.(*item)
		if !ok {
			q.List.Remove(e)

			continue
		}

		if accept != nil && !accept(i) {
			return nil
		}

		q.List.Remove(e)
		q.bytes -= i.size

		return i
	}

	return nil
}

// stats returns the queued bytes and when the item at the front was queued.
// Packets are kept in sequence order, so the front is the oldest one except
// for the occasional late, reordered packet.
func (q *queue) stats() (int, time.Time) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	front := q.Front()
	if front == nil {
		return 0, time.Time{}
	}

	i, _ := front.Value.(*item)

	return q.bytes, i.enqueuedAt
}

func isRetransmission(i *item) bool {
	return i.resend != nil
}

// LeakyBucketPacer implements a leaky bucket pacing algorithm, modelled on
// libwebrtc's PacingController:
//
//   - Packets are sent at DefaultPacingFactor times the congestion
//     controller's target bitrate.
//   - No packet waits longer than about DefaultMaxQueueTime: when the queue
//     would take longer to drain, the pacing rate is raised until it doesn't.
//   - Audio is not paced. It is sent on the next tick and counted against the
//     budget, so video slows down to make room for it.
//   - Retransmissions are sent before new video.
//
// Audio and RTX streams are only known when SetStreamInfo is called for them,
// see NewStreamInfoInterceptorFactory.
type LeakyBucketPacer struct {
	allowDuplicate    bool
	f                 float64
	targetBitrate     int
	targetBitrateLock sync.Mutex

	pacingInterval time.Duration
	maxQueueTime   time.Duration

	qLock  sync.RWMutex
	queues map[uint32]*queue
	// order lists the queues' SSRCs so drain can round-robin fairly;
	// nextQueue is the position to resume from on the next tick.
	order     []uint32
	nextQueue int
	done      chan struct{}
	closed    bool

	ssrcToWriter map[uint32]interceptor.RTPWriter
	// rtxToMedia maps an RTX SSRC to the SSRC of the stream it repairs.
	// RTX packets are written through that stream's writer.
	rtxToMedia map[uint32]uint32
	// audioSSRCs remembers audio streams that were announced before
	// AddStream created their queue.
	audioSSRCs map[uint32]bool
	writerLock sync.RWMutex
	log        logging.LeveledLogger
	rtppool    *rtppool.RTPPool
}

// NewLeakyBucketPacer initializes a new LeakyBucketPacer
func NewLeakyBucketPacer(log logging.LeveledLogger, initialBitrate int, allowDuplicate bool) *LeakyBucketPacer {
	return newLeakyBucketPacer(log, initialBitrate, allowDuplicate, DefaultMaxQueueTime)
}

// newLeakyBucketPacer is NewLeakyBucketPacer with a configurable queue time
// limit. A maxQueueTime of 0 disables the limit.
func newLeakyBucketPacer(
	log logging.LeveledLogger, initialBitrate int, allowDuplicate bool, maxQueueTime time.Duration,
) *LeakyBucketPacer {
	p := &LeakyBucketPacer{
		allowDuplicate: allowDuplicate,
		f:              DefaultPacingFactor,
		pacingInterval: 5 * time.Millisecond,
		maxQueueTime:   maxQueueTime,
		qLock:          sync.RWMutex{},
		done:           make(chan struct{}),
		ssrcToWriter:   map[uint32]interceptor.RTPWriter{},
		rtxToMedia:     map[uint32]uint32{},
		audioSSRCs:     map[uint32]bool{},
		queues:         map[uint32]*queue{},
		log:            log,
		rtppool:        rtppool.New(),
	}
	p.targetBitrate = int(p.f * float64(initialBitrate))

	go p.Run()
	return p
}

// AddStream adds a new stream and its corresponding writer to the pacer
func (p *LeakyBucketPacer) AddStream(ssrc uint32, writer interceptor.RTPWriter) {
	p.writerLock.Lock()
	p.ssrcToWriter[ssrc] = writer
	p.writerLock.Unlock()

	p.addQueue(ssrc)
}

// addQueue creates an empty queue for ssrc, replacing any existing one.
func (p *LeakyBucketPacer) addQueue(ssrc uint32) {
	p.writerLock.RLock()
	audio := p.audioSSRCs[ssrc]
	p.writerLock.RUnlock()

	p.qLock.Lock()
	defer p.qLock.Unlock()

	if p.closed {
		return
	}

	if _, ok := p.queues[ssrc]; !ok {
		p.order = append(p.order, ssrc)
	}
	p.queues[ssrc] = &queue{
		List:  list.List{},
		mu:    sync.RWMutex{},
		audio: audio,
	}
}

// SetStreamInfo tells the pacer what a local stream carries. Audio streams
// are not paced, and the stream's RTX SSRC, if any, gets its own queue that
// writes through the stream's writer. AddStream only gets the media SSRC, so
// without this the pacer would reject retransmissions sent on the RTX SSRC.
func (p *LeakyBucketPacer) SetStreamInfo(info *interceptor.StreamInfo) {
	audio := strings.HasPrefix(strings.ToLower(info.MimeType), "audio/")

	p.writerLock.Lock()
	if audio {
		p.audioSSRCs[info.SSRC] = true
	}
	rtx := info.SSRCRetransmission
	newRTX := rtx != 0 && rtx != info.SSRC && p.rtxToMedia[rtx] != info.SSRC
	if newRTX {
		p.rtxToMedia[rtx] = info.SSRC
	}
	p.writerLock.Unlock()

	if audio {
		p.qLock.Lock()
		if q, ok := p.queues[info.SSRC]; ok {
			q.audio = true
		}
		p.qLock.Unlock()
	}

	if newRTX {
		p.addQueue(rtx)
	}
}

// SetTargetBitrate updates the target bitrate at which the pacer is allowed to
// send packets. The pacer may exceed this limit by p.f
func (p *LeakyBucketPacer) SetTargetBitrate(rate int) {
	p.targetBitrateLock.Lock()
	defer p.targetBitrateLock.Unlock()
	p.targetBitrate = int(p.f * float64(rate))
}

// pacingBitrate returns the rate to send at: the target bitrate, raised when
// needed so the queue drains before its oldest packet is maxQueueTime old.
func (p *LeakyBucketPacer) pacingBitrate(now time.Time) int {
	bitrate := p.getTargetBitrate()
	if p.maxQueueTime <= 0 {
		return bitrate
	}

	queued, oldest := p.queueStats()
	if queued == 0 {
		return bitrate
	}

	// Once the oldest packet is overdue, flush the queue on this tick.
	timeLeft := max(p.pacingInterval, p.maxQueueTime-now.Sub(oldest))
	drain := int(float64(queued*8) / timeLeft.Seconds())

	return max(bitrate, drain)
}

// queueStats returns the bytes queued across all paced streams and when the
// oldest of them was queued. Audio queues are left out, they don't wait for
// budget.
func (p *LeakyBucketPacer) queueStats() (int, time.Time) {
	p.qLock.RLock()
	defer p.qLock.RUnlock()

	total := 0
	var oldest time.Time
	for _, q := range p.queues {
		if q.audio {
			continue
		}

		bytes, queuedAt := q.stats()
		if bytes == 0 {
			continue
		}

		total += bytes
		if oldest.IsZero() || queuedAt.Before(oldest) {
			oldest = queuedAt
		}
	}

	return total, oldest
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
		if media, isRTX := p.rtxToMedia[ssrc]; isRTX {
			writer, ok = p.ssrcToWriter[media]
		}
	}

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
		enqueuedAt: time.Now(),
	}
	if newItem.resend != nil {
		newItem.resend.Retain()
	}

	queue.mu.Lock()
	inserted := p.insert(queue, newItem)
	if inserted {
		queue.bytes += newItem.size
	}
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
			bitrate := p.pacingBitrate(now)
			budget += int(now.Sub(lastTick).Seconds() * float64(bitrate) / 8)
			lastTick = now

			budget = p.drain(budget)

			// Don't bank unlimited budget while idle, or the next frame
			// would be sent as one large burst.
			budget = min(budget, p.burstBytes(bitrate))
		}
	}
}

// drain sends the queued packets for one tick and returns the remaining
// budget. Audio goes first and is not limited by the budget, then
// retransmissions, then everything else. Paced streams are served
// round-robin, and the position carries over between ticks, so when only a
// packet or two fits per tick the streams still take turns. Only Run calls
// drain.
func (p *LeakyBucketPacer) drain(budget int) int {
	p.qLock.RLock()
	defer p.qLock.RUnlock()

	for _, ssrc := range p.order {
		queue := p.queues[ssrc]
		if !queue.audio {
			continue
		}

		for next := queue.popFront(nil); next != nil; next = queue.popFront(nil) {
			budget -= p.send(next)
		}
	}

	budget = p.roundRobin(budget, isRetransmission)

	return p.roundRobin(budget, nil)
}

// roundRobin sends the packets at the front of the paced queues that accept
// returns true for, until the budget is spent or no queue has one left. The
// caller must hold qLock.
func (p *LeakyBucketPacer) roundRobin(budget int, accept func(*item) bool) int {
	idle := 0
	for budget > 0 && idle < len(p.order) {
		if p.nextQueue >= len(p.order) {
			p.nextQueue = 0
		}
		queue := p.queues[p.order[p.nextQueue]]
		p.nextQueue++

		if queue.audio {
			idle++

			continue
		}

		next := queue.popFront(accept)
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
		// The transport closes before the pacer when a peer leaves.
		if !errors.Is(err, io.ErrClosedPipe) {
			p.log.Errorf("failed to write packet: %v", err)
		}
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
		queue.bytes = 0
		queue.mu.Unlock()

		delete(p.queues, ssrc)
	}
	p.order = nil

	close(p.done)

	return nil
}
