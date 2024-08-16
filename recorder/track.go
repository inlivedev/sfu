package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sync"
	"time"
)

type QuicTrack struct {
	TrackID   string
	ClientID  string
	RoomID    string
	stream    io.WriteCloser
	buffer    *bytes.Buffer
	cancelFn  context.CancelFunc
	cancelCtx context.Context
	mu        sync.Mutex
}

func NewTrack(conf *TrackConfig, stream io.WriteCloser) (*QuicTrack, error) {
	if err := validateTrackConfig(conf); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	track := &QuicTrack{
		TrackID:   conf.TrackID,
		ClientID:  conf.ClientID,
		RoomID:    conf.RoomID,
		stream:    stream,
		buffer:    bytes.NewBuffer(nil),
		cancelFn:  cancel,
		cancelCtx: ctx,
	}

	go track.startWriter()

	if err := track.sendNewTrackPacket(conf); err != nil {
		return nil, err
	}
	return track, nil
}

func (quicTrack *QuicTrack) startWriter() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-quicTrack.cancelCtx.Done():
			quicTrack.flush()
			return
		case <-ticker.C:
			quicTrack.flush()
		}
	}
}

func (quicTrack *QuicTrack) flush() {
	quicTrack.mu.Lock()
	defer quicTrack.mu.Unlock()

	if quicTrack.buffer.Len() > 0 {
		data := quicTrack.buffer.Bytes()
		if _, err := quicTrack.WriteRTP(data); err != nil {
			log.Printf("Error writing RTP data: %v", err)
		}
		quicTrack.buffer.Reset()
	}
}

func (quicTrack *QuicTrack) Write(p []byte) (int, error) {
	quicTrack.mu.Lock()
	defer quicTrack.mu.Unlock()
	return quicTrack.buffer.Write(p)
}

func (quicTrack *QuicTrack) WritePacket(p *StreamPacket) (int, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	return quicTrack.stream.Write(data)
}

func (quicTrack *QuicTrack) WriteRTP(data []byte) (int, error) {
	packet := &StreamPacket{
		PacketType: PacketTypeRTP,
		Payload:    data,
	}
	return quicTrack.WritePacket(packet)
}

func (quicTrack *QuicTrack) Close() error {
	quicTrack.flush()
	return quicTrack.stream.Close()
}

func (quicTrack *QuicTrack) sendNewTrackPacket(conf *TrackConfig) error {
	data, err := json.Marshal(conf)
	if err != nil {
		return err
	}

	packet := &StreamPacket{
		PacketType: PacketTypeNewTrack,
		Payload:    data,
	}

	_, err = quicTrack.WritePacket(packet)
	return err
}

func validateTrackConfig(conf *TrackConfig) error {
	if conf.TrackID == "" || conf.ClientID == "" || conf.RoomID == "" {
		return errors.New("invalid track configuration")
	}
	return nil
}
