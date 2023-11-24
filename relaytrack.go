package sfu

import (
	"errors"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type IRemoteTrack interface {
	ID() string
	RID() string
	PayloadType() webrtc.PayloadType
	Kind() webrtc.RTPCodecType
	StreamID() string
	SSRC() webrtc.SSRC
	Msid() string
	Codec() webrtc.RTPCodecParameters
	Read(b []byte) (n int, attributes interceptor.Attributes, err error)
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
	SetReadDeadline(deadline time.Time) error
}

// TrackRemote represents a single inbound source of media
type RelayTrack struct {
	mu sync.RWMutex

	id       string
	streamID string

	payloadType webrtc.PayloadType
	kind        webrtc.RTPCodecType
	ssrc        webrtc.SSRC
	mimeType    string
	rid         string
	rtpChan     chan *rtp.Packet
}

func NewTrackRelay(id, streamid, rid string, kind webrtc.RTPCodecType, ssrc webrtc.SSRC, mimeType string, rtpChan chan *rtp.Packet) IRemoteTrack {
	return &RelayTrack{
		id:       id,
		streamID: streamid,
		mimeType: mimeType,
		kind:     kind,
		ssrc:     ssrc,
		rid:      rid,
		rtpChan:  rtpChan,
	}
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (t *RelayTrack) ID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.id
}

// RID gets the RTP Stream ID of this Track
// With Simulcast you will have multiple tracks with the same ID, but different RID values.
// In many cases a TrackRemote will not have an RID, so it is important to assert it is non-zero
func (t *RelayTrack) RID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.rid
}

// PayloadType gets the PayloadType of the track
func (t *RelayTrack) PayloadType() webrtc.PayloadType {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.payloadType
}

// Kind gets the Kind of the track
func (t *RelayTrack) Kind() webrtc.RTPCodecType {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.kind
}

// StreamID is the group this track belongs too. This must be unique
func (t *RelayTrack) StreamID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.streamID
}

// SSRC gets the SSRC of the track
func (t *RelayTrack) SSRC() webrtc.SSRC {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ssrc
}

// Msid gets the Msid of the track
func (t *RelayTrack) Msid() string {
	return t.StreamID() + " " + t.ID()
}

// Codec gets the Codec of the track
func (t *RelayTrack) Codec() webrtc.RTPCodecParameters {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return getRTPParameters(t.mimeType)
}

// Read reads data from the track.
func (t *RelayTrack) Read(b []byte) (n int, attributes interceptor.Attributes, err error) {
	return 0, nil, errors.New("relaytrack: not implemented, use ReadRTP instead")
}

// ReadRTP is a convenience method that wraps Read and unmarshals for you.
func (t *RelayTrack) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	p := <-t.rtpChan
	return p, nil, nil
}

// SetReadDeadline sets the max amount of time the RTP stream will block before returning. 0 is forever.
func (t *RelayTrack) SetReadDeadline(deadline time.Time) error {
	return errors.New("relaytrack: not implemented")
}

// IsRelay returns true if this track is a relay track
func (t *RelayTrack) IsRelay() bool {
	return true
}
