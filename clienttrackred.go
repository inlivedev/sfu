package sfu

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

var (
	ErrIncompleteRedHeader = errors.New("util: incomplete RED header")
	ErrIncompleteRedBlock  = errors.New("util: incomplete RED block")
)

type clientTrackRed struct {
	id           string
	context      context.Context
	cancel       context.CancelFunc
	mu           sync.RWMutex
	client       *Client
	kind         webrtc.RTPCodecType
	mimeType     string
	localTrack   *webrtc.TrackLocalStaticRTP
	remoteTrack  *remoteTrack
	isReceiveRed bool
}

func (t *clientTrackRed) ID() string {
	return t.id
}

func (t *clientTrackRed) Context() context.Context {
	return t.context
}

func (t *clientTrackRed) Client() *Client {
	return t.client
}

func (t *clientTrackRed) Kind() webrtc.RTPCodecType {
	return t.remoteTrack.track.Kind()
}

func (t *clientTrackRed) push(rtp rtp.Packet, quality QualityLevel) {
	if t.client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	if !t.isReceiveRed {
		rtp = t.getPrimaryEncoding(rtp)
	}

	if err := t.localTrack.WriteRTP(&rtp); err != nil {
		glog.Error("clienttrack: error on write rtp", err)
	}
}

func (t *clientTrackRed) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *clientTrackRed) IsScreen() bool {
	return false
}

func (t *clientTrackRed) SetSourceType(_ TrackType) {
	// do nothing
}

func (t *clientTrackRed) IsSimulcast() bool {
	return false
}

func (t *clientTrackRed) IsScaleable() bool {
	return false
}

func (t *clientTrackRed) RequestPLI() {
	t.remoteTrack.sendPLI()
}

func (t *clientTrackRed) SetMaxQuality(_ QualityLevel) {
	// do nothing
}

func (t *clientTrackRed) MaxQuality() QualityLevel {
	return QualityHigh
}

func (t *clientTrackRed) getPrimaryEncoding(rtp rtp.Packet) rtp.Packet {
	payload, err := extractPrimaryEncodingForRED(rtp.Payload)
	if err != nil {
		glog.Error("clienttrack: error on extract primary encoding for red", err)
		return rtp
	}

	rtp.Payload = payload
	// set to opus payload type
	rtp.PayloadType = 111
	return rtp
}

// // Credit to Livekit
// // https://github.com/livekit/livekit/blob/56dd39968408f0973374e5b336a28606a1da79d2/pkg/sfu/redprimaryreceiver.go#L267
func extractPrimaryEncodingForRED(payload []byte) ([]byte, error) {

	/* RED payload https://datatracker.ietf.org/doc/html/rfc2198#section-3
		0                   1                    2                   3
	    0 1 2 3 4 5 6 7 8 9 0 1 2 3  4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |F|   block PT  |  timestamp offset         |   block length    |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   F: 1 bit First bit in header indicates whether another header block
	       follows.  If 1 further header blocks follow, if 0 this is the
	       last header block.
	*/

	var blockLength int
	for {
		if len(payload) < 1 {
			// illegal data, need at least one byte for primary encoding
			return nil, ErrIncompleteRedHeader
		}

		if payload[0]&0x80 == 0 {
			// last block is primary encoding data
			payload = payload[1:]
			break
		} else {
			if len(payload) < 4 {
				// illegal data
				return nil, ErrIncompleteRedHeader
			}

			blockLength += int(binary.BigEndian.Uint16(payload[2:]) & 0x03FF)
			payload = payload[4:]
		}
	}

	if len(payload) < blockLength {
		return nil, ErrIncompleteRedBlock
	}

	return payload[blockLength:], nil
}
