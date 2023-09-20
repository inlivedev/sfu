package sfu

import (
	"context"
	"io"
	"time"

	"github.com/golang/glog"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type RemoteTrack struct {
	context               context.Context
	track                 *webrtc.TrackRemote
	onRead                func(*rtp.Packet)
	onEnded               func()
	previousBytesReceived uint64
	currentBytesReceived  uint64
	currentBitrate        uint32
	latestUpdatedTS       time.Time
}

func NewRemoteTrack(ctx context.Context, track *webrtc.TrackRemote) *RemoteTrack {
	rt := &RemoteTrack{
		context: ctx,
		track:   track,
	}

	rt.readRTP()

	return rt
}

func (t *RemoteTrack) readRTP() {
	go func() {
		ctxx, cancel := context.WithCancel(t.context)

		defer cancel()

		for {
			select {
			case <-ctxx.Done():

				return
			default:
				rtp, _, readErr := t.track.ReadRTP()
				if readErr == io.EOF {
					if t.onEnded != nil {
						t.onEnded()
					}

					return
				} else if readErr != nil {
					glog.Error("error reading rtp: ", readErr.Error())
					return
				}

				if t.onRead != nil {
					t.onRead(rtp)
				}
			}
		}
	}()
}

func (t *RemoteTrack) updateStats(s stats.Stats) {
	if t.latestUpdatedTS.IsZero() {
		t.latestUpdatedTS = s.LastPacketReceivedTimestamp
		return
	}

	// update the bitrate if the last update equal or more than 1 second
	if s.LastPacketReceivedTimestamp.Sub(t.latestUpdatedTS) >= time.Second {
		t.latestUpdatedTS = s.LastPacketReceivedTimestamp
		t.previousBytesReceived = t.currentBytesReceived
		t.currentBytesReceived = s.BytesReceived
		t.currentBitrate = t.GetCurrentBitrate()
	}

}

func (t *RemoteTrack) Context() context.Context {
	return t.context
}

func (t *RemoteTrack) Track() *webrtc.TrackRemote {
	return t.track
}

func (t *RemoteTrack) GetCurrentBitrate() uint32 {
	if t.currentBytesReceived == 0 {
		return 0
	}

	return uint32((t.currentBytesReceived - t.previousBytesReceived) * 8)
}
