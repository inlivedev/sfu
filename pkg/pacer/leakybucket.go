// modified version of pion interceptor leaky_bucket_pacer.go
// https://github.com/pion/interceptor/blob/master/pkg/gcc/leaky_bucket_pacer.go
package pacer

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/samespace/sfu/pkg/rtppool"
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

func (q *queue) Remove(e *list.Element) *item {
	q.mu.Lock()
	defer q.mu.Unlock()

	i, ok := q.List.Remove(e).(*item)
	if !ok {
		return nil
	}

	return i
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
	done   chan struct{}

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

// Write sends a packet with header and payload the a previously registered
// stream.
func (p *LeakyBucketPacer) Write(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
	pkt := p.rtppool.NewPacket(header, payload)

	queue := p.getQueue(header.SSRC)
	if queue == nil {
		p.log.Errorf("no queue found for ssrc: ", header.SSRC)

		return 0, ErrNoQueueFound
	}

	newItem := &item{
		packet:     pkt,
		size:       len(payload),
		attributes: attributes,
	}

	// queue.PushBack(newItem)
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.Len() == 0 {
		queue.PushBack(newItem)

		return header.MarshalSize() + len(payload), nil
	}

Loop:
	for e := queue.Back(); e != nil; e = e.Prev() {

		currentCache, _ := e.Value.(*item)

		if currentCache.packet.Header().SequenceNumber == pkt.Header().SequenceNumber {
			if p.allowDuplicate {
				queue.InsertAfter(newItem, e)

				break Loop
			}

			p.log.Warnf("packet cache: packet sequence ", pkt.Header().SequenceNumber, " already exists in the cache, will not adding the packet")

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

		if e.Prev() == nil {
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
					queue.mu.RLock()
					if queue.Len() == 0 {
						queue.mu.RUnlock()
						emptyQueueCount++

						if emptyQueueCount == len(p.queues) {
							p.qLock.RUnlock()
							break Loop
						}

						continue
					}

					front := queue.Front()
					queue.mu.RUnlock()

					next := queue.Remove(front)
					if next == nil {
						p.log.Warnf("failed to access leaky bucket pacer queue, cast failed")
						emptyQueueCount++

						continue
					}

					writer := p.getWriter(next.packet.Header().SSRC)
					if writer == nil {
						p.log.Warnf("no writer found for ssrc: %v", next.packet.Header().SSRC)
						next.packet.Release()

						continue
					}

					// p.log.Info("sending packet SSRC ", next.packet.Header().SSRC, ": ", next.packet.Header().SequenceNumber)
					n, err := writer.Write(next.packet.Header(), next.packet.Payload(), next.attributes)
					if err != nil {
						p.log.Errorf("failed to write packet: ", err)
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
		queue.mu.RLock()
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
		queue.mu.RUnlock()

		queue.Init()
		delete(p.queues, ssrc)

	}

	close(p.done)

	return nil
}
