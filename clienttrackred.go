package sfu

import (
	"encoding/binary"
	"errors"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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
	var localTrack *webrtc.TrackLocalStaticRTP
	mimeType := t.remoteTrack.track.Codec().MimeType

	if !c.receiveRED {
		mimeType = webrtc.MimeTypeOpus
		localTrack = t.createOpusLocalTrack()
	} else {
		localTrack = t.createLocalTrack()
	}

	ctBase := newClientTrack(c, t, false, localTrack)
	ctBase.mimeType = mimeType

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
		primaryPacket := t.remoteTrack.rtppool.GetPacket()
		primaryPacket.Payload = t.getPrimaryEncoding(p.Payload[:len(p.Payload)])
		primaryPacket.Header = p.Header
		if err := t.localTrack.WriteRTP(primaryPacket); err != nil {
			t.client.log.Errorf("clienttrack: error on write primary rtp", err)
		}
		t.remoteTrack.rtppool.PutPacket(primaryPacket)
		return
	}

	if err := t.localTrack.WriteRTP(p); err != nil {
		t.client.log.Errorf("clienttrack: error on write rtp", err)
	}
}

func (t *clientTrackRed) getPrimaryEncoding(payload []byte) []byte {
	primaryPayload, err := extractPrimaryEncodingForRED(payload)
	if err != nil {
		t.client.log.Errorf("clienttrack: error on extract primary encoding for red", err)
		return payload
	}

	return primaryPayload
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

func (t *clientTrackRed) Quality() QualityLevel {
	return QualityAudioRed
}
