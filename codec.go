package sfu

import "github.com/pion/webrtc/v3"

func RegisterDefaultCodecs(m *webrtc.MediaEngine) error {
	// Default Pion Audio Codecs
	for _, codec := range []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeOpus, 48000, 2, "minptime=10;useinbandfec=1", nil},
			PayloadType:        111,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeG722, 8000, 0, "", nil},
			PayloadType:        9,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypePCMU, 8000, 0, "", nil},
			PayloadType:        0,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypePCMA, 8000, 0, "", nil},
			PayloadType:        8,
		},
	} {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	videoRTCPFeedback := []webrtc.RTCPFeedback{{"goog-remb", ""}, {"ccm", "fir"}, {"nack", ""}, {"nack", "pli"}}
	for _, codec := range []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeVP9, 90000, 0, "profile-id=2", videoRTCPFeedback},
			PayloadType:        100,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeVP9, 90000, 0, "profile-id=0", videoRTCPFeedback},
			PayloadType:        98,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", videoRTCPFeedback},
			PayloadType:        102,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f", videoRTCPFeedback},
			PayloadType:        104,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", videoRTCPFeedback},
			PayloadType:        106,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f", videoRTCPFeedback},
			PayloadType:        112,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f", videoRTCPFeedback},
			PayloadType:        108,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=4d001f", videoRTCPFeedback},
			PayloadType:        39,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeH264, 90000, 0, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f", videoRTCPFeedback},
			PayloadType:        127,
		},
	} {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}

	return nil
}
