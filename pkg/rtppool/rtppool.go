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
				return &rtp.Packet{
					Header:  rtp.Header{},
					Payload: make([]byte, 0),
				}
			},
		},

		PacketManager: pm,
	}
}

func (r *RTPPool) PutPacket(p *rtp.Packet) {
	p.Header = rtp.Header{}
	p.Payload = p.Payload[:0]

	r.pool.Put(p)
}

func (r *RTPPool) CopyPacket(p *rtp.Packet) *rtp.Packet {
	newPacket := r.GetPacket()
	newPacket.Header = p.Header.Clone()
	newPacket.Payload = append(newPacket.Payload, p.Payload...)

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
				buf := make([]byte, 0)
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
