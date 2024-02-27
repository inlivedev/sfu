package sfu

import (
	"testing"

	"github.com/pion/rtp"
)

func TestBufferPushOrdered(t *testing.T) {
	b := NewSendBuffer(1000)
	for i := 0; i < 1100; i++ {
		b.Push(&rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
			},
		})
	}

	if b.buffer.Len() != 1000 {
		t.Error("buffer length is not 1000")
	}

	if b.buffer.Front().Value.(*rtp.Packet).Header.SequenceNumber != 100 {
		t.Error("buffer front is not 100")
	}

	if b.buffer.Back().Value.(*rtp.Packet).Header.SequenceNumber != 1099 {
		t.Error("buffer back is not 1099")
	}
}

func TestBufferPushUnordered(t *testing.T) {
	b := NewSendBuffer(1000)
	seqs := []uint16{1, 3, 5, 7, 9, 2, 4, 6, 8, 10}
	for _, seq := range seqs {
		b.Push(&rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: seq,
			},
		})
	}

	if b.buffer.Len() != 10 {
		t.Error("buffer length is not 10")
	}

	if b.buffer.Front().Value.(*rtp.Packet).Header.SequenceNumber != 1 {
		t.Error("buffer front is not 1")
	}

	if b.buffer.Back().Value.(*rtp.Packet).Header.SequenceNumber != 10 {
		t.Error("buffer back is not 10")
	}

	for i := 1; i <= 10; i++ {
		if b.buffer.Front().Value.(*rtp.Packet).Header.SequenceNumber != uint16(i) {
			t.Errorf("buffer front is not %d, got %d", i, b.buffer.Front().Value.(*rtp.Packet).Header.SequenceNumber)
		}
		b.buffer.Remove(b.buffer.Front())
		if b.buffer.Len() != 10-i {
			t.Errorf("buffer length is not %d, got %d", 10-i-1, b.buffer.Len())
		}
	}
}

func TestBufferPop(t *testing.T) {
	b := NewSendBuffer(1000)
	for i := 0; i < 50; i++ {
		b.Push(&rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
			},
		})
	}

	for i := 0; i < 50; i++ {
		if b.Pop().Header.SequenceNumber != uint16(i) {
			t.Errorf("packet sequence is not %d, got %d", i, b.Pop().Header.SequenceNumber)
		}
	}

	if b.buffer.Len() != 0 {
		t.Error("buffer length is not 0")
	}
}
