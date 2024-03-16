package rtppool

import (
	"sync"

	"github.com/pion/rtp"
)

type rtpPool struct {
	pool sync.Pool
}

var RTPPacketPool = &rtpPool{
	pool: sync.Pool{
		New: func() interface{} {
			return &rtp.Packet{
				Header:  rtp.Header{},
				Payload: make([]byte, 1500),
			}
		},
	},
}

func (p *rtpPool) ResetPacketPoolAllocation(localPacket *rtp.Packet) {
	localPacket.Header = rtp.Header{}
	localPacket.Payload = localPacket.Payload[0:0]
	p.pool.Put(localPacket)
}

func (p *rtpPool) GetPacketAllocationFromPool() *rtp.Packet {
	ipacket := p.pool.Get()
	return ipacket.(*rtp.Packet) //nolint:forcetypeassert
}
