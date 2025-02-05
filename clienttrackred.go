package sfu

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var (
	ErrIncompleteRedHeader = errors.New("util: incomplete RED header")
	ErrIncompleteRedBlock  = errors.New("util: incomplete RED block")
)

type clientTrackRed struct {
	*clientTrackAudio
}

func newClientTrackRed(t *clientTrackAudio) *clientTrackRed {
	ct := &clientTrackRed{
		clientTrackAudio: t,
	}

	return ct
}

func (t *clientTrackRed) push(p *rtp.Packet, _ QualityLevel) {
	if t.client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	if !t.client.receiveRED {
		primaryPacket := t.remoteTrack.rtppool.CopyPacket(p)
		primaryPacket.Payload = t.getPrimaryEncoding(p.Payload)
		primaryPacket.Header = p.Header
		if err := t.localTrack.WriteRTP(primaryPacket); err != nil {
			t.client.log.Tracef("clienttrack: error on write primary rtp %s", err.Error())
		}
		t.remoteTrack.rtppool.PutPacket(primaryPacket)
	} else {
		if err := t.localTrack.WriteRTP(p); err != nil {
			t.client.log.Tracef("clienttrack: error on write rtp %s", err.Error())
		}
	}
}

func (t *clientTrackRed) getPrimaryEncoding(payload []byte) []byte {
	primaryPayload, _, err := ExtractRedPayloads(payload)
	if err != nil {
		t.client.log.Tracef("clienttrack: error on extract primary encoding for red %s", err.Error())
		return payload
	}

	return primaryPayload
}

func (t *clientTrackRed) Quality() QualityLevel {
	return QualityAudioRed
}

func (t *clientTrackRed) MaxQuality() QualityLevel {

	return QualityAudioRed
}

// extractRedPayloads extracts both primary and redundant payloads from a RED packet
// following RFC 2198 format
func ExtractRedPayloads(payload []byte) (primary []byte, redundant [][]byte, err error) {
	if len(payload) < 1 {
		return nil, nil, fmt.Errorf("illegal data, payload too short")
	}

	redundant = make([][]byte, 0)
	offset := 0

	var blockOffsets []int

	// First pass: collect all headers and validate
	for payload[offset]&0x80 != 0 { // while not at primary
		if len(payload[offset:]) < 4 {
			return nil, nil, fmt.Errorf("illegal data, truncated RED header")
		}

		blockLength := int(binary.BigEndian.Uint16(payload[offset+2:]) & 0x03FF)
		blockOffsets = append(blockOffsets, blockLength)
		offset += 4
	}

	// Skip primary header (1 byte)
	offset++
	if offset >= len(payload) {
		return nil, nil, fmt.Errorf("illegal data, no payload data")
	}

	// Calculate data start position for redundant blocks
	dataStart := offset

	// Extract redundant blocks
	currentPos := dataStart
	for _, length := range blockOffsets {
		if currentPos+length > len(payload) {
			return nil, nil, fmt.Errorf("illegal data, payload shorter than indicated length")
		}

		redundant = append(redundant, payload[currentPos:currentPos+length])

		currentPos += length
	}

	// The remaining data is the primary payload
	primary = payload[currentPos:]

	return primary, redundant, nil
}

// RedPacket holds the extracted primary and redundant RTP packets
type RedPacket struct {
	Primary   *rtp.Packet
	Redundant []*rtp.Packet
}

// extractRedPackets extracts both primary and redundant packets from a RED RTP packet
// following RFC 2198 format
func ExtractRedPackets(packet *rtp.Packet) (primary *rtp.Packet, redundants []*rtp.Packet, err error) {
	if len(packet.Payload) < 1 {
		return nil, nil, fmt.Errorf("illegal data, payload too short")
	}

	redundantPackets := make([]*rtp.Packet, 0)
	offset := 0
	var blockOffsets []int
	var timestampOffsets []uint16

	// First pass: collect all headers and validate
	for packet.Payload[offset]&0x80 != 0 { // while not at primary
		if len(packet.Payload[offset:]) < 4 {
			return nil, nil, fmt.Errorf("illegal data, truncated RED header")
		}

		// Extract timestamp offset (14 bits) and block length (10 bits)
		headerData := binary.BigEndian.Uint16(packet.Payload[offset+1:])
		tsOffset := headerData >> 2
		blockLength := int(binary.BigEndian.Uint16(packet.Payload[offset+2:]) & 0x03FF)

		blockOffsets = append(blockOffsets, blockLength)
		timestampOffsets = append(timestampOffsets, tsOffset)
		offset += 4
	}

	// Skip primary header (1 byte)
	offset++
	if offset >= len(packet.Payload) {
		return nil, nil, fmt.Errorf("illegal data, no payload data")
	}

	// Calculate data start position for redundant blocks
	dataStart := offset
	currentPos := dataStart

	// Extract redundant blocks and create RTP packets
	for i, length := range blockOffsets {
		if currentPos+length > len(packet.Payload) {
			return nil, nil, fmt.Errorf("illegal data, payload shorter than indicated length")
		}

		// Create new RTP packet for redundant data
		redundantPacket := &rtp.Packet{
			Header: rtp.Header{
				Version:        packet.Version,
				Padding:        packet.Padding,
				Extension:      packet.Extension,
				CSRC:           packet.CSRC,
				Marker:         packet.Marker,
				PayloadType:    packet.PayloadType,
				SequenceNumber: packet.SequenceNumber - uint16(i+1),
				Timestamp:      packet.Timestamp - uint32(timestampOffsets[i]),
				SSRC:           packet.SSRC,
			},
			Payload: packet.Payload[currentPos : currentPos+length],
		}

		redundantPackets = append(redundantPackets, redundantPacket)
		currentPos += length
	}

	// Create primary packet
	primaryPacket := &rtp.Packet{
		Header:  packet.Header,
		Payload: packet.Payload[currentPos:],
	}

	return primaryPacket, redundantPackets, nil
}
