// this code is from pion nack interceptor
// https://github.com/pion/interceptor/blob/master/pkg/nack/retainable_packet.go
package rtppool

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pion/rtp"
)

var (
	errPacketReleased          = errors.New("packet has been released")
	errFailedToCastHeaderPool  = errors.New("failed to cast header pool")
	errFailedToCastPayloadPool = errors.New("failed to cast payload pool")
)

const maxPayloadLen = 1460

type PacketManager struct {
	HeaderPool  *sync.Pool
	PayloadPool *sync.Pool
}

func newPacketManager() *PacketManager {
	return &PacketManager{
		HeaderPool: &sync.Pool{
			New: func() interface{} {
				return &rtp.Header{}
			},
		},
		PayloadPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, maxPayloadLen)
				return &buf
			},
		},
	}
}

func (m *PacketManager) NewPacket(header *rtp.Header, payload []byte) (*RetainablePacket, error) {
	if len(payload) > maxPayloadLen {
		return nil, io.ErrShortBuffer
	}

	p := &RetainablePacket{
		onRelease: m.releasePacket,
		// new packets have retain count of 1
		count:     1,
		addedTime: time.Now(),
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	var ok bool
	p.header, ok = m.HeaderPool.Get().(*rtp.Header)
	if !ok {
		return nil, errFailedToCastHeaderPool
	}

	*p.header = header.Clone()

	if payload != nil {
		p.buffer, ok = m.PayloadPool.Get().(*[]byte)
		if !ok {
			return nil, errFailedToCastPayloadPool
		}

		size := copy(*p.buffer, payload)
		p.payload = (*p.buffer)[:size]
	}

	return p, nil
}

func (m *PacketManager) releasePacket(header *rtp.Header, payload *[]byte) {
	m.HeaderPool.Put(header)
	if payload != nil {
		m.PayloadPool.Put(payload)
	}
}

type RetainablePacket struct {
	onRelease func(*rtp.Header, *[]byte)
	mu        sync.RWMutex
	count     int

	header    *rtp.Header
	buffer    *[]byte
	payload   []byte
	addedTime time.Time
}

func (p *RetainablePacket) Header() *rtp.Header {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.header
}

func (p *RetainablePacket) Payload() []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.payload
}

func (p *RetainablePacket) AddedTime() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.addedTime
}

func (p *RetainablePacket) Retain() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.count == 0 {
		// already released
		return errPacketReleased
	}
	p.count++
	return nil
}

func (p *RetainablePacket) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count--

	if p.count == 0 {
		// release back to pool
		p.onRelease(p.header, p.buffer)
		p.header = nil
		p.buffer = nil
		p.payload = nil
	}
}
