package recorder

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/quic-go/quic-go"
)

type PacketType byte

const (
	ConfigPacket   PacketType = 0x01
	DataPacket     PacketType = 0x02
	RoomEndPacket  PacketType = 0x03
	TrackEndPacket PacketType = 0x04
)

type TrackConfig struct {
	TrackID  string
	ClientID string
	RoomID   string
	FileName string
	MimeType string
}

type TrackRecorder interface {
	Write(data []byte) (int, error)
	WritePacket(packet *rtp.Packet) (int, error)
	Close() error
}

type QuicTrack struct {
	TrackID        string
	ClientID       string
	RoomID         string
	quicStream     quic.SendStream
	mu             sync.Mutex
	isConfigPacket atomic.Bool
}

func NewQuickTrackRecorder(conf *TrackConfig, stream quic.SendStream) (TrackRecorder, error) {
	if err := validateTrackConfig(conf); err != nil {
		return nil, err
	}

	track := &QuicTrack{
		TrackID:    conf.TrackID,
		ClientID:   conf.ClientID,
		RoomID:     conf.RoomID,
		quicStream: stream,
		mu:         sync.Mutex{},
	}

	track.isConfigPacket.Store(true)

	err := track.sendNewTrackPacket(conf)
	if err != nil {
		return nil, err
	}

	return track, nil
}

func (q *QuicTrack) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	var packetType PacketType
	if q.isConfigPacket.Load() {
		packetType = ConfigPacket
		q.isConfigPacket.Store(false)
	} else {
		packetType = DataPacket
	}

	header := make([]byte, 3)
	header[0] = byte(packetType)
	binary.BigEndian.PutUint16(header[1:], uint16(len(p)))

	packet := append(header, p...)
	n, err := q.quicStream.Write(packet)
	if err != nil {
		return 0, err
	}

	if n != len(packet) {
		return n - len(header), io.ErrShortWrite
	}

	return len(p), nil
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
	_, err := q.Write([]byte{byte(TrackEndPacket)})
	return err
}
func (q *QuicTrack) Close() error {
	err := q.sendTrackEndPacket()
	if err != nil {
		return err
	}
	return q.quicStream.Close()
}

func validateTrackConfig(conf *TrackConfig) error {
	if conf.TrackID == "" || conf.ClientID == "" || conf.RoomID == "" {
		return errors.New("invalid track configuration")
	}
	return nil
}
