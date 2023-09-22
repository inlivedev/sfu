package sfu

import (
	"context"
	"io"
	"sync"
	"time"

	"sync/atomic"

	"github.com/golang/glog"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type RemoteTrack struct {
	client                *Client
	mu                    sync.Mutex
	track                 *webrtc.TrackRemote
	onRead                func(*rtp.Packet)
	previousBytesReceived atomic.Uint64
	currentBytesReceived  atomic.Uint64
	latestUpdatedTS       atomic.Uint64
}

func NewRemoteTrack(client *Client, track *webrtc.TrackRemote, onRead func(*rtp.Packet)) *RemoteTrack {
	rt := &RemoteTrack{
		client: client,
		mu:     sync.Mutex{},
		track:  track,
		onRead: onRead,
	}

	rt.readRTP()

	return rt
}

func (t *RemoteTrack) onEnded() {
	t.client.tracks.Remove([]string{t.track.ID()})
	trackIDs := make([]string, 0)
	trackIDs = append(trackIDs, t.track.ID())

	t.client.sfu.removeTracks(trackIDs)
}

func (t *RemoteTrack) readRTP() {
	go func() {
		ctxx, cancel := context.WithCancel(t.client.context)

		defer cancel()

		for {
			select {
			case <-ctxx.Done():

				return
			default:
				rtp, _, readErr := t.track.ReadRTP()
				if readErr == io.EOF {
					t.onEnded()

					return
				} else if readErr != nil {
					glog.Error("error reading rtp: ", readErr.Error())
					return
				}

				t.onRead(rtp)

				go t.client.updateReceiverStats(t)

			}
		}
	}()
}

func (t *RemoteTrack) updateStats(s stats.Stats) {
	if t.latestUpdatedTS.Load() == 0 {
		t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))
		return
	}

	// update the bitrate if the last update equal or more than 1 second
	lastTS := time.Unix(0, int64(t.latestUpdatedTS.Load()))
	if time.Since(lastTS) >= time.Second {
		t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))
		current := t.currentBytesReceived.Load()
		t.previousBytesReceived.Store(current)
		t.currentBytesReceived.Store(s.BytesReceived)
	}

}

func (t *RemoteTrack) Track() *webrtc.TrackRemote {
	return t.track
}

func (t *RemoteTrack) GetCurrentBitrate() uint32 {
	if t.currentBytesReceived.Load() == 0 {
		return 0
	}

	current := t.currentBytesReceived.Load()
	previous := t.previousBytesReceived.Load()

	return uint32((current - previous) * 8)
}
