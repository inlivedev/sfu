package rtppool

import (
	"sync"

	"github.com/pion/rtp"
)

type RTPPool struct {
	pool          sync.Pool
	PacketManager *PacketManager
}

var blankPayload = make([]byte, maxPayloadLen)

func New() *RTPPool {
	return &RTPPool{
		pool: sync.Pool{
			New: func() interface{} {
				return &rtp.Packet{}
			},
		},
		PacketManager: NewPacketManager(),
	}
}

func (r *RTPPool) PutPacket(localPacket *rtp.Packet) {

	localPacket.Header = rtp.Header{}
	copy(localPacket.Payload, blankPayload)

	r.pool.Put(localPacket)
}

func (r *RTPPool) GetPacket() *rtp.Packet {
	ipacket := r.pool.Get()
	return ipacket.(*rtp.Packet) //nolint:forcetypeassert
}

func (r *RTPPool) GetPayload() *[]byte {
	ipayload := r.PacketManager.PayloadPool.Get()
	return ipayload.(*[]byte) //nolint:forcetypeassert
}

func (r *RTPPool) PutPayload(localPayload *[]byte) {
	copy(*localPayload, blankPayload)
	r.PacketManager.PayloadPool.Put(localPayload)
}

func (r *RTPPool) NewPacket(header *rtp.Header, payload []byte) *RetainablePacket {
	pkt, err := r.PacketManager.NewPacket(header, payload)
	if err != nil {
		return nil
	}

	return pkt
}

type BufferPPool struct {
	pool *sync.Pool
}

func NewBufferPool() *BufferPPool {
	return &BufferPPool{
		pool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, maxPayloadLen)
				return &buf
			},
		},
	}
}

func (r *BufferPPool) Get() *[]byte {
	ipayload := r.pool.Get()
	return ipayload.(*[]byte) //nolint:forcetypeassert
}

func (r *BufferPPool) Put(localPayload *[]byte) {
	copy(*localPayload, blankPayload)
	r.pool.Put(localPayload)
}
