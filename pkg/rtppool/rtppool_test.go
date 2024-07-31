package rtppool

import (
	"testing"

	"github.com/pion/rtp"
)

var testPacket = &rtp.Packet{
	Header:  rtp.Header{},
	Payload: make([]byte, 1400),
}
var header = &rtp.Header{}
var payload = make([]byte, 1400)

func BenchmarkSlicePool(b *testing.B) {

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := GetPacketAllocationFromPool()

		p = testPacket

		ResetPacketPoolAllocation(p)
	}
}

func BenchmarkPacketManager(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, _ := rtpPacketPool.PacketManager.NewPacket(header, payload)

		p.Release()
	}
}
