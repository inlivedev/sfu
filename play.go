package sfu

import (
	"fmt"

	"github.com/pion/webrtc/v4"
)

func (c *Client) GetPlayer(done chan bool) (*AudioPlayer, error) {
	t, err := webrtc.NewTrackLocalStaticSample(
		getCodecCapability(webrtc.MimeTypeOpus),
		"audio",
		"audio-player",
	)
	if err != nil {
		return nil, fmt.Errorf("error creating rtp track : %w", err)
	}
	rtpSender, err := c.peerConnection.AddTrack(t)
	if err != nil {
		return nil, fmt.Errorf("error adding track to peer connection : %w", err)
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	c.renegotiate()
	return newAudioPlayer(t, done), nil
}
