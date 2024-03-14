package sfu

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/jaevor/go-nanoid"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/turn/v2"
	"github.com/pion/webrtc/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const (
	SdesRepairRTPStreamIDURI = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"
	uint16SizeHalf           = uint16(1 << 15)
)

var customChars = [62]byte{
	'A', 'B', 'C', 'D', 'E',
	'F', 'G', 'H', 'I', 'J',
	'K', 'L', 'M', 'N', 'O',
	'P', 'Q', 'R', 'S', 'T',
	'U', 'V', 'W', 'X', 'Y',
	'Z', 'a', 'b', 'c', 'd',
	'e', 'f', 'g', 'h', 'i',
	'j', 'k', 'l', 'm', 'n',
	'o', 'p', 'q', 'r', 's',
	't', 'u', 'v', 'w', 'x',
	'y', 'z', '0', '1', '2',
	'3', '4', '5', '6', '7',
	'8', '9',
}

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

func GenerateID(length int) string {
	canonicID, err := nanoid.CustomASCII(string(customChars[:]), length)
	if err != nil {
		panic(err)
	}

	return canonicID()
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

func StartTurnServer(ctx context.Context, publicIP string) *turn.Server {
	port := 3478
	users := "user=pass"
	realm := "test"
	threadNum := 1
	flag.Parse()

	if len(publicIP) == 0 {
		log.Fatalf("'public-ip' is required")
	}

	addr, err := net.ResolveUDPAddr("udp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		log.Fatalf("Failed to parse server address: %s", err)
	}

	// Cache -users flag for easy lookup later
	// If passwords are stored they should be saved to your DB hashed using turn.GenerateAuthKey
	usersMap := map[string][]byte{}
	for _, kv := range regexp.MustCompile(`(\w+)=(\w+)`).FindAllStringSubmatch(users, -1) {
		usersMap[kv[1]] = turn.GenerateAuthKey(kv[1], realm, kv[2])
	}

	// Create `numThreads` UDP listeners to pass into pion/turn
	// pion/turn itself doesn't allocate any UDP sockets, but lets the user pass them in
	// this allows us to add logging, storage or modify inbound/outbound traffic
	// UDP listeners share the same local address:port with setting SO_REUSEPORT and the kernel
	// will load-balance received packets per the IP 5-tuple
	listenerConfig := &net.ListenConfig{
		Control: func(network, address string, conn syscall.RawConn) error {
			return setSocketOptions(network, address, conn)
		},
	}

	relayAddressGenerator := &turn.RelayAddressGeneratorStatic{
		RelayAddress: net.ParseIP(publicIP), // Claim that we are listening on IP passed by user
		Address:      "0.0.0.0",             // But actually be listening on every interface
	}

	packetConnConfigs := make([]turn.PacketConnConfig, threadNum)
	for i := 0; i < threadNum; i++ {
		conn, listErr := listenerConfig.ListenPacket(ctx, addr.Network(), addr.String())
		if listErr != nil {
			log.Fatalf("Failed to allocate UDP listener at %s:%s", addr.Network(), addr.String())
		}

		packetConnConfigs[i] = turn.PacketConnConfig{
			PacketConn:            conn,
			RelayAddressGenerator: relayAddressGenerator,
		}

		log.Printf("Server %d listening on %s\n", i, conn.LocalAddr().String())
	}

	s, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		// Set AuthHandler callback
		// This is called every time a user tries to authenticate with the TURN server
		// Return the key for that user, or false when no user is found
		AuthHandler: func(username string, realm string, srcAddr net.Addr) ([]byte, bool) {
			if key, ok := usersMap[username]; ok {
				return key, true
			}
			return nil, false
		},
		// PacketConnConfigs is a list of UDP Listeners and the configuration around them
		PacketConnConfigs: packetConnConfigs,
	})
	if err != nil {
		log.Panicf("Failed to create TURN server: %s", err)
	}

	return s
}

func GetLocalIp() (net.IP, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}

	var ip net.IP

	for _, addr := range addresses {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil && ipnet.IP.IsPrivate() {
				ip = ipnet.IP
			}
		}
	}
	return ip, nil
}

func FlattenErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}

	errString := ""
	for _, err := range errs {
		errString += err.Error() + "\n"
	}

	return errors.New(errString)
}

func Uint32ToQualityLevel(quality uint32) QualityLevel {
	switch quality {
	case 0:
		return QualityNone
	case 1:
		return QualityLow
	case 2:
		return QualityMid
	case 3:
		return QualityHigh
	case 4:
		return QualityAudio
	case 5:
		return QualityAudioRed
	default:
		return QualityLow
	}
}

func ThousandSeparator(n int) string {
	p := message.NewPrinter(language.English)
	return p.Sprintf("%d", n)
}

func IsRTPPacketLate(packetSeqNum uint16, lastSeqNum uint16) bool {
	return lastSeqNum > packetSeqNum && lastSeqNum-packetSeqNum < uint16SizeHalf
}

func copyRTPPacket(packet *rtp.Packet) *rtp.Packet {
	newPacket := &rtp.Packet{}
	*newPacket = *packet
	newPacket.Payload = make([]byte, len(packet.Payload))
	copy(newPacket.Payload, packet.Payload)
	return newPacket
}
