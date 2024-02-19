package sfu

import (
	"encoding/binary"
	"strings"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"golang.org/x/exp/slices"
)

var (
	videoRTCPFeedback = []webrtc.RTCPFeedback{{"goog-remb", ""}, {"ccm", "fir"}, {"nack", ""}, {"nack", "pli"}}

	videoCodecs = []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeVP9, 90000, 0, "profile-id=2", videoRTCPFeedback},
			PayloadType:        100,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=100", nil},
			PayloadType:        101,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeVP9, 90000, 0, "profile-id=0", videoRTCPFeedback},
			PayloadType:        98,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=98", nil},
			PayloadType:        99,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", videoRTCPFeedback},
			PayloadType:        102,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=102", nil},
			PayloadType:        103,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f", videoRTCPFeedback},
			PayloadType:        104,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=104", nil},
			PayloadType:        105,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", videoRTCPFeedback},
			PayloadType:        106,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=106", nil},
			PayloadType:        107,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f", videoRTCPFeedback},
			PayloadType:        112,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=112", nil},
			PayloadType:        113,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f", videoRTCPFeedback},
			PayloadType:        108,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=108", nil},
			PayloadType:        109,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=4d001f", videoRTCPFeedback},
			PayloadType:        39,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=39", nil},
			PayloadType:        40,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f", videoRTCPFeedback},
			PayloadType:        127,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=127", nil},
			PayloadType:        125,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeVP8, 90000, 0, "", videoRTCPFeedback},
			PayloadType:        96,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"video/rtx", 90000, 0, "apt=96", nil},
			PayloadType:        97,
		},
	}

	audioCodecs = []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{"audio/red", 48000, 2, "111/111", nil},
			PayloadType:        63,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeOpus, 48000, 2, "minptime=10;useinbandfec=1", nil},
			PayloadType:        111,
		},
	}

	H264KeyFrame2x2SPS = []byte{
		0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88,
		0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00,
		0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
	}
	H264KeyFrame2x2PPS = []byte{
		0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20,
	}
	H264KeyFrame2x2IDR = []byte{
		0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00,
		0xa7, 0xbe,
	}
	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}

	OpusSilenceFrame = []byte{
		0xf8, 0xff, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

func RegisterCodecs(m *webrtc.MediaEngine, codecs []string) error {
	errors := []error{}

	for _, codec := range audioCodecs {
		if slices.Contains(codecs, codec.MimeType) {
			if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
				errors = append(errors, err)
			}
		}
	}

	registeredVideoCodecs := make([]webrtc.RTPCodecParameters, 0)

	for _, codec := range videoCodecs {

		if slices.Contains(codecs, codec.MimeType) {
			if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
				errors = append(errors, err)
			}

			registeredVideoCodecs = append(registeredVideoCodecs, codec)
		}
	}

	for _, codec := range registeredVideoCodecs {
		for _, videoCodec := range videoCodecs {
			if videoCodec.RTPCodecCapability.MimeType == "video/rtx" && videoCodec.RTPCodecCapability.SDPFmtpLine == "apt="+string(codec.PayloadType) {
				if err := m.RegisterCodec(videoCodec, webrtc.RTPCodecTypeVideo); err != nil {
					errors = append(errors, err)
				}
			}
		}
	}

	return FlattenErrors(errors)
}

func RegisterDefaultCodecs(m *webrtc.MediaEngine) error {
	// Default Pion Audio Codecs
	for _, codec := range audioCodecs {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	for _, codec := range videoCodecs {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}

	return nil
}

// credit to Livekit code
// taken from https://github.com/livekit/livekit/blob/989d621c9746f1912fc44f8f8021a0388fdd773f/pkg/sfu/downtrack.go#L1429
func getH264BlankFrame() []byte {

	buf := make([]byte, 1000)
	offset := 0
	buf[0] = 0x18 // STAP-A
	offset++
	for _, payload := range H264KeyFrame2x2 {
		binary.BigEndian.PutUint16(buf[offset:], uint16(len(payload)))
		offset += 2
		copy(buf[offset:offset+len(payload)], payload)
		offset += len(payload)
	}

	return buf[:offset]
}

// reuse from pion media engine and media sample
func payloaderForCodec(codec webrtc.RTPCodecCapability) (rtp.Payloader, error) {
	switch strings.ToLower(codec.MimeType) {
	case strings.ToLower(webrtc.MimeTypeH264):
		return &codecs.H264Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeOpus):
		return &codecs.OpusPayloader{}, nil
	case strings.ToLower(webrtc.MimeTypeVP8):
		return &codecs.VP8Payloader{
			EnablePictureID: true,
		}, nil
	case strings.ToLower(webrtc.MimeTypeVP9):
		return &codecs.VP9Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeAV1):
		return &codecs.AV1Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeG722):
		return &codecs.G722Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypePCMU), strings.ToLower(webrtc.MimeTypePCMA):
		return &codecs.G711Payloader{}, nil
	default:
		return nil, webrtc.ErrNoPayloaderForCodec
	}
}

func SendBlackImageFrames(startSequence uint16, localRTP *webrtc.TrackLocalStaticRTP, sample *media.Sample) (uint16, error) {
	sequencer := rtp.NewFixedSequencer(startSequence)
	payloader, _ := payloaderForCodec(localRTP.Codec())
	p := rtp.NewPacketizer(
		1450,
		0, // Value is handled when writing
		0, // Value is handled when writing
		payloader,
		sequencer,
		localRTP.Codec().ClockRate,
	)

	clockRate := localRTP.Codec().ClockRate

	lastSequenceNumber := startSequence

	// skip packets by the number of previously dropped packets
	for i := uint16(0); i < sample.PrevDroppedPackets; i++ {
		lastSequenceNumber = sequencer.NextSequenceNumber()
	}

	samples := uint32(sample.Duration.Seconds()) * clockRate
	if sample.PrevDroppedPackets > 0 {
		p.SkipSamples(samples * uint32(sample.PrevDroppedPackets))
	}

	packets := p.Packetize(sample.Data, samples)

	writeErrs := []error{}
	for _, p := range packets {
		if err := localRTP.WriteRTP(p); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	return lastSequenceNumber + uint16(len(packets)), FlattenErrors(writeErrs)
}

func getPayloadType(mimeType string) webrtc.PayloadType {
	for _, codec := range audioCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec.PayloadType
		}
	}

	for _, codec := range videoCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec.PayloadType
		}
	}

	return 0
}

func getRTPParameters(mimeType string) webrtc.RTPCodecParameters {
	for _, codec := range audioCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec
		}
	}

	for _, codec := range videoCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec
		}
	}

	return webrtc.RTPCodecParameters{}
}

func getCodecCapability(mimeType string) webrtc.RTPCodecCapability {
	for _, codec := range audioCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec.RTPCodecCapability
		}
	}

	for _, codec := range videoCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec.RTPCodecCapability
		}
	}

	return webrtc.RTPCodecCapability{}
}
