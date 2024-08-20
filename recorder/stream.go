package recorder

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
)

type PacketType byte

const (
	ConfigPacket PacketType = 0x01
	DataPacket   PacketType = 0x02
)

type QuicStream struct {
	mu             sync.Mutex
	isConfigPacket atomic.Bool
	quicStream     quic.SendStream
}

func NewQuicStream(quicStream quic.SendStream) io.WriteCloser {
	s := &QuicStream{
		quicStream:     quicStream,
		isConfigPacket: atomic.Bool{},
	}
	s.isConfigPacket.Store(true)
	return s
}

func (q *QuicStream) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	var packetType PacketType
	if q.isConfigPacket.Load() {
		packetType = ConfigPacket
		q.isConfigPacket.Store(false)
	} else {
		packetType = DataPacket
	}

	fmt.Println("Sending packet type", packetType)
	fmt.Println("Original payload length", len(p))

	header := make([]byte, 3)
	header[0] = byte(packetType)
	binary.BigEndian.PutUint16(header[1:], uint16(len(p)))

	packet := append(header, p...)

	fmt.Println("Sending packet with length ", len(packet), "with header length", len(header), "and payload length", uint16(len(p)))

	fmt.Println("Sending packet", string(packet))

	n, err := q.quicStream.Write(packet)
	if err != nil {
		return 0, err
	}

	if n != len(packet) {
		return n - len(header), io.ErrShortWrite
	}
	return len(p), nil
}

func (q *QuicStream) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.quicStream.Close()
}
