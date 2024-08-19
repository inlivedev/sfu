package sfu

import (
	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/pion/webrtc/v4"
)

type clientTrackAudio struct {
	*clientTrack
}

func newClientTrackAudio(c *Client, t ITrack) *clientTrackAudio {
	var localTrack *webrtc.TrackLocalStaticRTP
	audioTrack, ok := t.(*AudioTrack)
	if !ok {
		c.log.Errorf("clienttrackaudio: track is not audio")
		return nil
	}

	if !c.receiveRED {
		localTrack = audioTrack.createOpusLocalTrack()
	} else {
		localTrack = audioTrack.createLocalTrack()
	}
	ctBase := newClientTrack(c, audioTrack.Track, false, localTrack)
	cta := &clientTrackAudio{
		clientTrack: ctBase,
	}

	track := t.(*AudioTrack)
	if c.options.EnableVoiceDetection {
		track.OnVoiceDetected(func(pkts []voiceactivedetector.VoicePacketData) {
			activity := voiceactivedetector.VoiceActivity{
				TrackID:     cta.id,
				StreamID:    cta.streamid,
				SSRC:        uint32(cta.ssrc),
				ClockRate:   cta.baseTrack.codec.ClockRate,
				AudioLevels: pkts,
			}

			c.onVoiceSentDetected(activity)
		})
	}

	return cta
}
