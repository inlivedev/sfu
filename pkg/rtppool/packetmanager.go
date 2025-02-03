// this code is from pion nack interceptor
// https://github.com/pion/interceptor/blob/master/pkg/nack/retainable_packet.go
package rtppool

import (
	"errors"
	"io"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

var (
	errPacketReleased          = errors.New("packet has been released")
	errFailedToCastPacketPool  = errors.New("failed to cast packet pool")
	errFailedToCastHeaderPool  = errors.New("failed to cast header pool")
	errFailedToCastPayloadPool = errors.New("failed to cast payload pool")
)

const maxPayloadLen = 1460

type PacketManager struct {
	PacketPool  *sync.Pool
	HeaderPool  *sync.Pool
	PayloadPool *sync.Pool
	AttrPool    *sync.Pool
}

func NewPacketManager() *PacketManager {
	return &PacketManager{
		PacketPool: &sync.Pool{
			New: func() interface{} {
				return &RetainablePacket{}
			},
		},

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

		AttrPool: &sync.Pool{
			New: func() interface{} {
				return interceptor.Attributes{}
			},
		},
	}
}

func (m *PacketManager) NewPacket(header *rtp.Header, payload []byte, attr interceptor.Attributes) (*RetainablePacket, error) {
	if len(payload) > maxPayloadLen {
		return nil, io.ErrShortBuffer
	}

	var ok bool

	p, ok := m.PacketPool.Get().(*RetainablePacket)
	if !ok {
		return nil, errFailedToCastPacketPool
	}

	p.onRelease = m.releasePacket
	p.count = 1

	p.mu.Lock()
	defer p.mu.Unlock()

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

	if attr != nil {
		p.attr, ok = m.AttrPool.Get().(interceptor.Attributes)
		if !ok {
			return nil, errFailedToCastPayloadPool
		}

		// copy map
		for k, v := range attr {
			p.attr[k] = v
		}
	}

	return p, nil
}

func (m *PacketManager) releasePacket(header *rtp.Header, payload *[]byte, p *RetainablePacket) {
	m.HeaderPool.Put(header)
	if payload != nil {
		*payload = (*payload)[:0]
		m.PayloadPool.Put(payload)
	}

	if p.attr != nil {
		// clear map
		for k := range p.attr {
			delete(p.attr, k)
		}

		m.AttrPool.Put(p.attr)
	}

	m.PacketPool.Put(p)
}

type RetainablePacket struct {
	onRelease func(*rtp.Header, *[]byte, *RetainablePacket)
	mu        sync.RWMutex
	count     int

	header  *rtp.Header
	buffer  *[]byte
	payload []byte
	attr    interceptor.Attributes
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

func (p *RetainablePacket) Attributes() interceptor.Attributes {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.attr
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
		p.onRelease(p.header, p.buffer, p)
	}
}
