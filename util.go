package sfu

import (
	"bufio"
	"math/rand"
	"strings"

	"github.com/pion/interceptor/pkg/stats"
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
