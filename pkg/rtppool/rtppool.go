package rtppool

import (
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type RTPPool struct {
	pool          sync.Pool
	PacketManager *PacketManager
}

func New() *RTPPool {
	pm := NewPacketManager()

	return &RTPPool{
		pool: sync.Pool{
			New: func() interface{} {
				return &rtp.Packet{}
			},
		},

		PacketManager: pm,
	}
}

func (r *RTPPool) PutPacket(p *rtp.Packet) {
	*p = rtp.Packet{}
	r.pool.Put(p)
}

// CopyPacket creates a copy of the RTP packet using the pool.
// The returned packet MUST be returned to the pool via PutPacket when done.
// WARNING: Do not use the returned packet across goroutine boundaries without additional synchronization.
// WARNING: Do not hold references to the packet after calling PutPacket.
func (r *RTPPool) CopyPacket(p *rtp.Packet) *rtp.Packet {
	newPacket := r.GetPacket()
	*newPacket = *p

	return newPacket
}

func (r *RTPPool) GetPayload() *[]byte {
	return r.PacketManager.PayloadPool.Get()
}

func (r *RTPPool) PutPayload(localPayload *[]byte) {
	*localPayload = (*localPayload)[:0]
	r.PacketManager.PayloadPool.Put(localPayload)
}

func (r *RTPPool) NewPacket(header *rtp.Header, payload []byte, attr interceptor.Attributes) *RetainablePacket {
	pkt, err := r.PacketManager.NewPacket(header, payload, attr)
	if err != nil {
		return nil
	}

	return pkt
}

func (r *RTPPool) GetPacket() *rtp.Packet {
	return r.pool.Get().(*rtp.Packet)
}

type BufferPool struct {
	pool *sync.Pool
}

func NewBufferPool() *BufferPool {
	return &BufferPool{
		pool: &sync.Pool{
			New: func() interface{} {
				// Initialize with a reasonable default capacity, e.g., 1500 bytes
				buf := make([]byte, 0, 1500)
				return &buf
			},
		},
	}
}

func (r *BufferPool) Get() *[]byte {
	ipayload := r.pool.Get()
	return ipayload.(*[]byte) //nolint:forcetypeassert
}

func (r *BufferPool) Put(localPayload *[]byte) {
	*localPayload = (*localPayload)[:0]

	r.pool.Put(localPayload)
}
