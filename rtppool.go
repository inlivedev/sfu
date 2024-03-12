package sfu

import (
	"sync"

	"github.com/pion/rtp"
)

type rtpPool struct {
	pool sync.Pool
}

var rtpPacketPool = &rtpPool{
	pool: sync.Pool{
		New: func() interface{} {
			return &rtp.Packet{}
		},
	},
}

func (p *rtpPool) ResetPacketPoolAllocation(localPacket *rtp.Packet) {
	*localPacket = rtp.Packet{}
	p.pool.Put(localPacket)
}

func (p *rtpPool) GetPacketAllocationFromPool() *rtp.Packet {
	ipacket := p.pool.Get()
	return ipacket.(*rtp.Packet) //nolint:forcetypeassert
}
