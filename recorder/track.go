package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/pion/rtp"
)

type TrackRecorder interface {
	Write(data []byte) (int, error)
	WritePacket(packet *rtp.Packet) (int, error)
	Close() error
}

type QuicTrack struct {
	TrackID   string
	ClientID  string
	RoomID    string
	stream    io.WriteCloser
	buffer    bytes.Buffer
	cancelFn  context.CancelFunc
	cancelCtx context.Context
	mu        sync.Mutex
}

func NewQuickTrackRecorder(conf *TrackConfig, stream io.WriteCloser) (TrackRecorder, error) {
	if err := validateTrackConfig(conf); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	track := &QuicTrack{
		TrackID:   conf.TrackID,
		ClientID:  conf.ClientID,
		RoomID:    conf.RoomID,
		stream:    stream,
		buffer:    bytes.Buffer{},
		cancelFn:  cancel,
		cancelCtx: ctx,
		mu:        sync.Mutex{},
	}

	err := track.sendNewTrackPacket(conf)
	if err != nil {
		return nil, err
	}

	return track, nil
}

func (q *QuicTrack) Write(data []byte) (int, error) {
	return q.stream.Write(data)
}

func (q *QuicTrack) WritePacket(packet *rtp.Packet) (int, error) {
	p, err := packet.Marshal()
	if err != nil {
		return 0, err
	}
	return q.Write(p)
}

func (q *QuicTrack) Close() error {
	return q.stream.Close()
}

func (q *QuicTrack) sendNewTrackPacket(conf *TrackConfig) error {
	data, err := json.Marshal(conf)
	if err != nil {
		return err
	}

	_, err = q.Write(data)
	return err
}

func validateTrackConfig(conf *TrackConfig) error {
	if conf.TrackID == "" || conf.ClientID == "" || conf.RoomID == "" {
		return errors.New("invalid track configuration")
	}
	return nil
}
