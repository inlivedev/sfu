package sfu

import (
	"bufio"
	"math/rand"
	"strings"

	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/speps/go-hashids"
)

const (
	SdesRepairRTPStreamIDURI = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"
)

func GetUfragAndPass(sdp string) (ufrag, pass string) {
	scanner := bufio.NewScanner(strings.NewReader(sdp))
	iceUfrag := "a=ice-ufrag:"
	icePwd := "a=ice-pwd:" //nolint:gosec //it's not a password

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, iceUfrag) {
			ufrag = strings.Replace(line, iceUfrag, "", 1)
		} else if strings.Contains(line, icePwd) {
			pass = strings.Replace(line, icePwd, "", 1)
		}

		if ufrag != "" && pass != "" {
			break
		}
	}

	return ufrag, pass
}

func CountTracks(sdp string) int {
	counter := 0

	scanner := bufio.NewScanner(strings.NewReader(sdp))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "m=audio") || strings.Contains(line, "m=video") {
			counter++
		}
	}

	return counter
}

func GenerateID(data []int) string {
	randInt := rand.Intn(100) //nolint:gosec //it's not a password
	data = append(data, randInt)
	hd := hashids.NewData()
	hd.Salt = "this is my salt"
	hd.MinLength = 9
	h, _ := hashids.NewWithData(hd)
	e, _ := h.Encode(data)

	return e
}

func GetReceiverStats(pc *webrtc.PeerConnection, statsGetter stats.Getter) map[webrtc.SSRC]stats.Stats {
	stats := make(map[webrtc.SSRC]stats.Stats)
	for _, t := range pc.GetTransceivers() {
		if t.Receiver() != nil && t.Receiver().Track() != nil {
			stats[t.Receiver().Track().SSRC()] = *statsGetter.Get(uint32(t.Receiver().Track().SSRC()))
		}
	}

	return stats
}

func GetSenderStats(pc *webrtc.PeerConnection, statsGetter stats.Getter) map[webrtc.SSRC]stats.Stats {
	stats := make(map[webrtc.SSRC]stats.Stats)
	for _, t := range pc.GetTransceivers() {
		if t.Sender() != nil && t.Sender().Track() != nil {
			ssrc := t.Sender().GetParameters().Encodings[0].SSRC
			stats[ssrc] = *statsGetter.Get(uint32(ssrc))
		}
	}

	return stats
}

func RegisterSimulcastHeaderExtensions(m *webrtc.MediaEngine, codecType webrtc.RTPCodecType) {
	for _, extension := range []string{
		sdp.SDESMidURI,
		sdp.SDESRTPStreamIDURI,
		SdesRepairRTPStreamIDURI,
	} {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: extension}, codecType); err != nil {
			panic(err)
		}
	}
}

func IsKeyframe(codec string, packet *rtp.Packet) bool {
	isIt1, isIt2 := Keyframe(codec, packet)
	return isIt1 && isIt2
}

// This keyframe related code is taken from https://github.com/jech/galene/blob/cc2ed144843ef5ebc4353b7096708355cab3b992/codecs/codecs.go#L17
// all credits belongs to Juliusz Chroboczek @jech the creator of Galene
//
// Keyframe determines if packet is the start of a keyframe.
// It returns (true, true) if that is the case, (false, true) if that is
// definitely not the case, and (false, false) if the information cannot
// be determined.
func Keyframe(codec string, packet *rtp.Packet) (bool, bool) {
	if strings.EqualFold(codec, "video/vp8") {
		var vp8 codecs.VP8Packet
		_, err := vp8.Unmarshal(packet.Payload)
		if err != nil || len(vp8.Payload) < 1 {
			return false, false
		}

		if vp8.S != 0 && vp8.PID == 0 && (vp8.Payload[0]&0x1) == 0 {
			return true, true
		}
		return false, true
	} else if strings.EqualFold(codec, "video/vp9") {
		var vp9 codecs.VP9Packet
		_, err := vp9.Unmarshal(packet.Payload)
		if err != nil || len(vp9.Payload) < 1 {
			return false, false
		}
		if !vp9.B {
			return false, true
		}

		if (vp9.Payload[0] & 0xc0) != 0x80 {
			return false, false
		}

		profile := (vp9.Payload[0] >> 4) & 0x3
		if profile != 3 {
			return (vp9.Payload[0] & 0xC) == 0, true
		}
		return (vp9.Payload[0] & 0x6) == 0, true
	} else if strings.EqualFold(codec, "video/av1") {
		if len(packet.Payload) < 2 {
			return false, true
		}
		// Z=0, N=1
		if (packet.Payload[0] & 0x88) != 0x08 {
			return false, true
		}
		w := (packet.Payload[0] & 0x30) >> 4

		getObu := func(data []byte, last bool) ([]byte, int, bool) {
			if last {
				return data, len(data), false
			}
			offset := 0
			length := 0
			for {
				if len(data) <= offset {
					return nil, offset, offset > 0
				}
				l := data[offset]
				length |= int(l&0x7f) << (offset * 7)
				offset++
				if (l & 0x80) == 0 {
					break
				}
			}
			if len(data) < offset+length {
				return data[offset:], len(data), true
			}
			return data[offset : offset+length],
				offset + length, false
		}
		offset := 1
		i := 0
		for {
			obu, length, truncated :=
				getObu(packet.Payload[offset:], int(w) == i+1)
			if len(obu) < 1 {
				return false, false
			}
			tpe := (obu[0] & 0x38) >> 3
			switch i {
			case 0:
				// OBU_SEQUENCE_HEADER
				if tpe != 1 {
					return false, true
				}
			default:
				// OBU_FRAME_HEADER or OBU_FRAME
				if tpe == 3 || tpe == 6 {
					if len(obu) < 2 {
						return false, false
					}
					// show_existing_frame == 0
					if (obu[1] & 0x80) != 0 {
						return false, true
					}
					// frame_type == KEY_FRAME
					return (obu[1] & 0x60) == 0, true
				}
			}
			if truncated || i >= int(w) {
				// the first frame header is in a second
				// packet, give up.
				return false, false
			}
			offset += length
			i++
		}
	} else if strings.EqualFold(codec, "video/h264") {
		if len(packet.Payload) < 1 {
			return false, false
		}
		nalu := packet.Payload[0] & 0x1F
		if nalu == 0 {
			// reserved
			return false, false
		} else if nalu <= 23 {
			// simple NALU
			return nalu == 7, true
		} else if nalu == 24 || nalu == 25 || nalu == 26 || nalu == 27 {
			// STAP-A, STAP-B, MTAP16 or MTAP24
			i := 1
			if nalu == 25 || nalu == 26 || nalu == 27 {
				// skip DON
				i += 2
			}
			for i < len(packet.Payload) {
				if i+2 > len(packet.Payload) {
					return false, false
				}
				length := uint16(packet.Payload[i])<<8 |
					uint16(packet.Payload[i+1])
				i += 2
				if i+int(length) > len(packet.Payload) {
					return false, false
				}
				offset := 0
				if nalu == 26 {
					offset = 3
				} else if nalu == 27 {
					offset = 4
				}
				if offset >= int(length) {
					return false, false
				}
				n := packet.Payload[i+offset] & 0x1F
				if n == 7 {
					return true, true
				} else if n >= 24 {
					// is this legal?
					return false, false
				}
				i += int(length)
			}
			if i == len(packet.Payload) {
				return false, true
			}
			return false, false
		} else if nalu == 28 || nalu == 29 {
			// FU-A or FU-B
			if len(packet.Payload) < 2 {
				return false, false
			}
			if (packet.Payload[1] & 0x80) == 0 {
				// not a starting fragment
				return false, true
			}
			return (packet.Payload[1]&0x1F == 7), true
		}
		return false, false
	}
	return false, false
}

func KeyframeDimensions(codec string, packet *rtp.Packet) (uint32, uint32) {
	if strings.EqualFold(codec, "video/vp8") {
		var vp8 codecs.VP8Packet
		_, err := vp8.Unmarshal(packet.Payload)
		if err != nil {
			return 0, 0
		}
		if len(vp8.Payload) < 10 {
			return 0, 0
		}
		raw := uint32(vp8.Payload[6]) | uint32(vp8.Payload[7])<<8 |
			uint32(vp8.Payload[8])<<16 | uint32(vp8.Payload[9])<<24
		width := raw & 0x3FFF
		height := (raw >> 16) & 0x3FFF
		return width, height
	} else if strings.EqualFold(codec, "video/vp9") {
		if packet == nil {
			return 0, 0
		}
		var vp9 codecs.VP9Packet
		_, err := vp9.Unmarshal(packet.Payload)
		if err != nil {
			return 0, 0
		}
		if !vp9.V {
			return 0, 0
		}
		w := uint32(0)
		h := uint32(0)
		for i := range vp9.Width {
			if i >= len(vp9.Height) {
				break
			}
			if w < uint32(vp9.Width[i]) {
				w = uint32(vp9.Width[i])
			}
			if h < uint32(vp9.Height[i]) {
				h = uint32(vp9.Height[i])
			}
		}
		return w, h
	} else {
		return 0, 0
	}
}
