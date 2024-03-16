package sfu

import (
	"encoding/binary"
	"errors"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

var (
	ErrIncompleteRedHeader = errors.New("util: incomplete RED header")
	ErrIncompleteRedBlock  = errors.New("util: incomplete RED block")
)

type clientTrackRed struct {
	*clientTrack
	isReceiveRed bool
}

func newClientTrackRed(c *Client, t *Track) *clientTrackRed {

	mimeType := t.remoteTrack.track.Codec().MimeType
	localTrack := t.createLocalTrack()

	if !c.receiveRED {
		mimeType = webrtc.MimeTypeOpus
		localTrack = t.createOpusLocalTrack()
	}

	ctBase := newClientTrack(c, t, false)
	ctBase.mimeType = mimeType
	ctBase.localTrack = localTrack

	ct := &clientTrackRed{
		clientTrack:  ctBase,
		isReceiveRed: c.receiveRED,
	}

	return ct
}

func (t *clientTrackRed) push(p *rtp.Packet, _ QualityLevel) {
	if t.client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	if !t.isReceiveRed {
		p = t.getPrimaryEncoding(p)
	}

	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("clienttrack: error on write rtp", err)
	}
}

func (t *clientTrackRed) getPrimaryEncoding(p *rtp.Packet) *rtp.Packet {
	payload, err := extractPrimaryEncodingForRED(p.Payload)
	if err != nil {
		glog.Error("clienttrack: error on extract primary encoding for red", err)
		return p
	}

	p.Payload = payload
	// set to opus payload type
	p.PayloadType = 111
	return p
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
