package recorder

import (
	"io"
	"sync"

	"github.com/quic-go/quic-go"
)

type QuicStream struct {
	mu         sync.Mutex
	quicStream quic.SendStream
}

func NewQuicStream(quicStream quic.SendStream) io.WriteCloser {
	return &QuicStream{
		quicStream: quicStream,
	}
}

func (q *QuicStream) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	n, err := q.quicStream.Write(p)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func (q *QuicStream) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.quicStream.Close()
}
