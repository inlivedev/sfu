package rtppool

import (
	"sync"

	"github.com/pion/rtp"
)

type rtpPool struct {
	pool          sync.Pool
	packetManager *packetManager
}

var rtpPacetPool = &rtpPool{
	pool: sync.Pool{
		New: func() interface{} {
			return &rtp.Packet{}
		},
	},
	packetManager: newPacketManager(),
}

func ResetPacketPoolAllocation(localPacket *rtp.Packet) {

	localPacket.Header = rtp.Header{}
	localPacket.Payload = localPacket.Payload[:0]

	rtpPacetPool.pool.Put(localPacket)
}

func GetPacketAllocationFromPool() *rtp.Packet {
	ipacket := rtpPacetPool.pool.Get()
	return ipacket.(*rtp.Packet) //nolint:forcetypeassert
}

func NewPacket(header *rtp.Header, payload []byte) *RetainablePacket {
	pkt, err := rtpPacetPool.packetManager.NewPacket(header, payload)
	if err != nil {
		return nil
	}

	return pkt
}
