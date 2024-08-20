package recorder

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/pion/rtp"
)

type TrackConfig struct {
	TrackID  string
	ClientID string
	RoomID   string
	MimeType string
}

type TrackRecorder interface {
	Write(data []byte) (int, error)
	WritePacket(packet *rtp.Packet) (int, error)
	Close() error
}

type QuicTrack struct {
	TrackID  string
	ClientID string
	RoomID   string
	stream   io.WriteCloser
}

func NewQuickTrackRecorder(conf *TrackConfig, stream io.WriteCloser) (TrackRecorder, error) {
	if err := validateTrackConfig(conf); err != nil {
		return nil, err
	}

	track := &QuicTrack{
		TrackID:  conf.TrackID,
		ClientID: conf.ClientID,
		RoomID:   conf.RoomID,
		stream:   stream,
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

func (q *QuicTrack) sendNewTrackPacket(conf *TrackConfig) error {
	data, err := json.Marshal(conf)
	if err != nil {
		return err
	}

	_, err = q.Write(data)
	return err
}

func (q *QuicTrack) sendTrackEndPacket() error {
	_, err := q.Write([]byte{0x00})
	return err
}
func (q *QuicTrack) Close() error {
	err := q.sendTrackEndPacket()
	if err != nil {
		return err
	}
	return q.stream.Close()
}

func validateTrackConfig(conf *TrackConfig) error {
	if conf.TrackID == "" || conf.ClientID == "" || conf.RoomID == "" {
		return errors.New("invalid track configuration")
	}
	return nil
}
