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

var errNoQueueFound = errors.New("no queue found for ssrc")

type item struct {
	packet     *rtppool.RetainablePacket
	size       int
	attributes interceptor.Attributes
}

type queue struct {
	list.List
	mu sync.RWMutex
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

// Write sends a packet with header and payload the a previously registered
// stream.
func (p *LeakyBucketPacer) Write(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
	pkt := rtppool.NewPacket(header, payload)

	p.qLock.RLock()
	queue, ok := p.queues[header.SSRC]
	p.qLock.RUnlock()

	// could be that the queue was cleaned up for closing the pacer
	if queue == nil {
		return 0, nil
	}

	queue.mu.Lock()

	if !ok {
		glog.Error("no queue found for ssrc: ", header.SSRC)
		return 0, errNoQueueFound
	}

	queue.PushBack(&item{
		packet:     pkt,
		size:       len(payload),
		attributes: attributes,
	})

	queue.mu.Unlock()

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

				for _, queue := range p.queues {
					queue.mu.RLock()
					queueSize := queue.Len()
					queue.mu.RUnlock()
					if queueSize == 0 {
						emptyQueueCount++

						if emptyQueueCount == len(p.queues) {
							break Loop
						}

						continue
					}

					queue.mu.Lock()
					next, ok := queue.Remove(queue.Front()).(*item)
					queue.mu.Unlock()

					if !ok {
						glog.Warningf("failed to access leaky bucket pacer queue, cast failed")
						continue
					}

					p.writerLock.RLock()
					writer, ok := p.ssrcToWriter[next.packet.Header().SSRC]
					p.writerLock.RUnlock()

					if !ok {
						glog.Warningf("no writer found for ssrc: %v", next.packet.Header().SSRC)
						next.packet.Release()
						continue
					}

					n, err := writer.Write(next.packet.Header(), next.packet.Payload(), next.attributes)
					if err != nil {
						glog.Error("failed to write packet: ", err)
					}

					lastSent = now
					budget -= n

					next.packet.Release()
				}
			}

		}
	}
}

// Close closes the LeakyBucketPacer
func (p *LeakyBucketPacer) Close() error {
	p.qLock.Lock()
	defer p.qLock.Unlock()
	for ssrc, queue := range p.queues {
		queue.mu.Lock()
		for e := queue.Front(); e != nil; e = e.Next() {
			packet := e.Value.(*item).packet
			packet.Release()
		}

		queue.Init()
		delete(p.queues, ssrc)
		queue.mu.Unlock()
	}

	close(p.done)

	return nil
}
