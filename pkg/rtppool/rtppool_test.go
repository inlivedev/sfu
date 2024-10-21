package rtppool

import (
	"context"
	"io"
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
	var pool = New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := pool.GetPacket()

		pool.PutPacket(p)
	}
}

func BenchmarkPacketManager(b *testing.B) {
	var pool = New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, _ := pool.PacketManager.NewPacket(header, payload, nil)

		p.Release()
	}
}

func BenchmarkPayloadPool(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := NewBufferPool()

	pipeReader, pipeWriter := io.Pipe()

	go func() {
		p := []byte{0x80, 0x60, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00}
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, _ = pipeWriter.Write(p)
			}
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := pool.Get()

		i, err := pipeReader.Read(*p)
		if err != nil {
			b.Errorf("Read failed: %v", err)
			b.FailNow()
			return
		}

		if i != 8 {
			b.Errorf("Length read %d not same with write ", i)
			b.FailNow()
			return
		}

		pool.Put(p)
	}
}
