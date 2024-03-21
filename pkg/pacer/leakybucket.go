// modified version of pion interceptor leaky_bucket_pacer.go
// https://github.com/pion/interceptor/blob/master/pkg/gcc/leaky_bucket_pacer.go
package pacer

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

var (
	ErrNoQueueFound = errors.New("no queue found for ssrc")
	ErrDuplicate    = errors.New("packet sequence already exists in the queue")
)

const (
	uint16SizeHalf = uint16(1 << 15)
)

type item struct {
	packet     *rtppool.RetainablePacket
	size       int
	attributes interceptor.Attributes
}

type queue struct {
	list.List
	mu sync.RWMutex
}

func (q *queue) Front() *list.Element {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.List.Front()
}

func (q *queue) Back() *list.Element {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.List.Back()
}

func (q *queue) Len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.List.Len()
}

func (q *queue) PushBack(v *item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.List.PushBack(v)
}

func (q *queue) PushFront(v *item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.List.PushFront(v)
}

func (q *queue) Remove(e *list.Element) *item {
	q.mu.Lock()
	defer q.mu.Unlock()

	i, ok := q.List.Remove(e).(*item)
	if !ok {
		return nil
	}

	return i
}

func (q *queue) InsertAfter(v *item, mark *list.Element) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.List.InsertAfter(v, mark)
}

// LeakyBucketPacer implements a leaky bucket pacing algorithm
type LeakyBucketPacer struct {
	f                 float64
	targetBitrate     int
	targetBitrateLock sync.Mutex

	pacingInterval time.Duration

	qLock  sync.RWMutex
	queues map[uint32]*queue
	done   chan struct{}

	ssrcToWriter map[uint32]interceptor.RTPWriter
	writerLock   sync.RWMutex
}

// NewLeakyBucketPacer initializes a new LeakyBucketPacer
func NewLeakyBucketPacer(initialBitrate int) *LeakyBucketPacer {
	p := &LeakyBucketPacer{
		f:              1.5,
		targetBitrate:  initialBitrate,
		pacingInterval: 5 * time.Millisecond,
		qLock:          sync.RWMutex{},
		done:           make(chan struct{}),
		ssrcToWriter:   map[uint32]interceptor.RTPWriter{},
		queues:         map[uint32]*queue{},
	}

	go p.Run()
	return p
}

// AddStream adds a new stream and its corresponding writer to the pacer
func (p *LeakyBucketPacer) AddStream(ssrc uint32, writer interceptor.RTPWriter) {
	p.writerLock.Lock()
	defer p.writerLock.Unlock()

	p.qLock.Lock()
	defer p.qLock.Unlock()

	p.ssrcToWriter[ssrc] = writer
	p.queues[ssrc] = &queue{
		List: list.List{},
		mu:   sync.RWMutex{},
	}
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
		glog.Warningf("no writer found for ssrc: %v", ssrc)
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

// Write sends a packet with header and payload the a previously registered
// stream.
func (p *LeakyBucketPacer) Write(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
	pkt := rtppool.NewPacket(header, payload)
	err := pkt.Retain()
	if err != nil {
		glog.Warning("failed to retain packet: ", err)
		return 0, err
	}

	queue := p.getQueue(header.SSRC)
	if queue == nil {
		glog.Error("no queue found for ssrc: ", header.SSRC)

		return 0, ErrNoQueueFound
	}

	newItem := &item{
		packet:     pkt,
		size:       len(payload),
		attributes: attributes,
	}

	if queue.Len() == 0 {
		queue.PushBack(newItem)
		return header.MarshalSize() + len(payload), nil
	}

	// add packet in order
	e := queue.Back()
Loop:
	for {
		if e == nil {
			break Loop
		}

		currentCache, ok := e.Value.(*item)
		if !ok {
			// could be because the item already removed
			e = e.Prev()
			if e == nil {
				break Loop
			}

			continue
		}

		if currentCache.packet.Header().SequenceNumber == pkt.Header().SequenceNumber {
			glog.Warning("packet cache: packet sequence ", pkt.Header().SequenceNumber, " already exists in the cache, will not adding the packet")

			return 0, ErrDuplicate
		}

		if currentCache.packet.Header().SequenceNumber < pkt.Header().SequenceNumber && pkt.Header().SequenceNumber-currentCache.packet.Header().SequenceNumber < uint16SizeHalf {
			queue.InsertAfter(newItem, e)

			break Loop
		}

		if currentCache.packet.Header().SequenceNumber-pkt.Header().SequenceNumber > uint16SizeHalf {
			queue.InsertAfter(newItem, e)

			break Loop
		}

		queue.mu.RLock()
		e := e.Prev()
		queue.mu.RUnlock()

		if e == nil {
			queue.PushFront(newItem)

			break Loop
		}

	}

	return header.MarshalSize() + len(payload), nil
}

// Run starts the LeakyBucketPacer
func (p *LeakyBucketPacer) Run() {
	ticker := time.NewTicker(p.pacingInterval)
	defer ticker.Stop()

	lastSent := time.Now()
	for {
		select {
		case <-p.done:
			return
		case now := <-ticker.C:
			budget := int(float64(now.Sub(lastSent).Milliseconds()) * float64(p.getTargetBitrate()) / 8000.0)
		Loop:
			for {
				emptyQueueCount := 0

				p.qLock.RLock()
				if len(p.queues) == 0 {
					p.qLock.RUnlock()
					break Loop
				}

				for _, queue := range p.queues {
					if queue.Len() == 0 {
						emptyQueueCount++

						if emptyQueueCount == len(p.queues) {
							p.qLock.RUnlock()
							break Loop
						}

						continue
					}

					next := queue.Remove(queue.Front())
					if next == nil {
						glog.Warningf("failed to access leaky bucket pacer queue, cast failed")
						emptyQueueCount++

						continue
					}

					writer := p.getWriter(next.packet.Header().SSRC)
					if writer == nil {
						glog.Warningf("no writer found for ssrc: %v", next.packet.Header().SSRC)
						next.packet.Release()

						continue
					}

					glog.Info("sending packet SSRC ", next.packet.Header().SSRC, ": ", next.packet.Header().SequenceNumber)
					n, err := writer.Write(next.packet.Header(), next.packet.Payload(), next.attributes)
					if err != nil {
						glog.Error("failed to write packet: ", err)
					}

					lastSent = now
					budget -= n

					next.packet.Release()
				}
				p.qLock.RUnlock()

			}

		}
	}
}

// Close closes the LeakyBucketPacer
func (p *LeakyBucketPacer) Close() error {
	p.qLock.Lock()
	defer p.qLock.Unlock()
	for ssrc, queue := range p.queues {
		for e := queue.Front(); e != nil; e = e.Next() {
			i, ok := e.Value.(*item)
			if !ok {
				continue
			}

			writer := p.getWriter(ssrc)
			if writer != nil {
				_, _ = writer.Write(i.packet.Header(), i.packet.Payload(), nil)
			}

			i.packet.Release()
		}

		queue.Init()
		delete(p.queues, ssrc)

	}

	close(p.done)

	return nil
}
