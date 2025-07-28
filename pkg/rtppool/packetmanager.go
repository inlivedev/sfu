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
	errPacketReleased         = errors.New("packet has been released")
	errFailedToCastPacketPool = errors.New("failed to cast packet pool")
)

const maxPayloadLen = 1460

type PacketManager struct {
	PacketPool  *sync.Pool
	PayloadPool *BufferPool
}

func NewPacketManager() *PacketManager {
	m := &PacketManager{}
	m.PayloadPool = NewBufferPool()

	m.PacketPool = &sync.Pool{
		New: func() interface{} {
			payloadBuffer := m.PayloadPool.Get()
			return &RetainablePacket{
				header:  &rtp.Header{},
				payload: *payloadBuffer,
				attrMap: make(interceptor.Attributes),
				manager: m,
			}
		},
	}

	return m
}

func (m *PacketManager) NewPacket(header *rtp.Header, payload []byte, attr interceptor.Attributes) (*RetainablePacket, error) {
	if len(payload) > maxPayloadLen {
		return nil, io.ErrShortBuffer
	}

	p, ok := m.PacketPool.Get().(*RetainablePacket)
	if !ok {
		return nil, errFailedToCastPacketPool
	}

	p.count = 1

	p.mu.Lock()
	defer p.mu.Unlock()

	*p.header = header.Clone()

	if payload != nil {
		if cap(p.payload) < len(payload) {
			p.payload = make([]byte, len(payload))
		} else {
			p.payload = p.payload[:len(payload)]
		}
		copy(p.payload, payload)
	} else {
		p.payload = p.payload[:0]
	}

	if p.attrMap == nil {
		p.attrMap = make(interceptor.Attributes)
	}
	for k, v := range attr {
		p.attrMap[k] = v
	}

	return p, nil
}

type RetainablePacket struct {
	mu      sync.RWMutex
	count   int
	manager *PacketManager
	header  *rtp.Header
	payload []byte
	attrMap interceptor.Attributes
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

	return p.attrMap
}

func (p *RetainablePacket) Retain() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.count == 0 {
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
		*p.header = rtp.Header{}
		p.payload = p.payload[:0]

		for k := range p.attrMap {
			delete(p.attrMap, k)
		}
		p.manager.PacketPool.Put(p)
	}
}
