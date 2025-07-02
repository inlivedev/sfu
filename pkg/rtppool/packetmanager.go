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
	PayloadPool *BufferPool
}

func NewPacketManager() *PacketManager {
	m := &PacketManager{}
	m.PayloadPool = NewBufferPool()

	m.PacketPool = &sync.Pool{
		New: func() interface{} {
			payload := m.PayloadPool.Get() // payload is *[]byte
			return &RetainablePacket{
				header:  &rtp.Header{},
				payload: *payload,                     // dereferences to []byte, copies slice header
				attrMap: make(interceptor.Attributes), // Revert to make for map type
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

	var ok bool

	p, ok := m.PacketPool.Get().(*RetainablePacket)
	if !ok {
		return nil, errFailedToCastPacketPool
	}

	p.count = 1

	p.mu.Lock()
	defer p.mu.Unlock()

	*p.header = header.Clone()

	if payload != nil {
		copy(p.payload, payload)
	}

	// Direct map copy
	if p.attrMap == nil { // Ensure map is initialized, though pool should do it.
		p.attrMap = make(interceptor.Attributes)
	}
	for k, v := range attr { // attr is also map[string]interface{}
		p.attrMap[k] = v // Correct assignment for a map
	}

	return p, nil
}

type RetainablePacket struct {
	mu      sync.RWMutex
	count   int
	manager *PacketManager
	header  *rtp.Header
	payload []byte
	attrMap interceptor.Attributes // Change type from interceptor.Attributes to underlying type
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

	// Return the map directly since interceptor.Attributes is just map[string]interface{}
	return p.attrMap
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
		*p.header = rtp.Header{} // Reset header

		p.payload = p.payload[:0] // Reset payload slice length

		// Reset attributes
		for k := range p.attrMap { // p.attrMap is map[string]interface{}
			delete(p.attrMap, k) // Correct way to clear a map
		}
		p.manager.PacketPool.Put(p) // Return RetainablePacket to its pool
	}
}
