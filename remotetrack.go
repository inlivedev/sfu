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
	bitrate               *atomic.Uint32
	previousBytesReceived *atomic.Uint64
	currentBytesReceived  *atomic.Uint64
	latestUpdatedTS       *atomic.Uint64
	onEndedCallbacks      []func()
}

func NewRemoteTrack(client *Client, track *webrtc.TrackRemote, onRead func(*rtp.Packet)) *RemoteTrack {
	rt := &RemoteTrack{
		client:                client,
		mu:                    sync.Mutex{},
		track:                 track,
		onRead:                onRead,
		bitrate:               &atomic.Uint32{},
		previousBytesReceived: &atomic.Uint64{},
		currentBytesReceived:  &atomic.Uint64{},
		latestUpdatedTS:       &atomic.Uint64{},
		onEndedCallbacks:      make([]func(), 0),
	}

	rt.readRTP()

	return rt
}

func (t *RemoteTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *RemoteTrack) onEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.client.tracks.Remove([]string{t.track.ID()})
	trackIDs := make([]string, 0)
	trackIDs = append(trackIDs, t.track.ID())

	t.client.sfu.removeTracks(trackIDs)

	for _, f := range t.onEndedCallbacks {
		f()
	}
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

				readDoneChan := make(chan bool)
				go func() {
					timeout, cancelTimeout := context.WithTimeout(ctxx, 5*time.Second)
					defer cancelTimeout()
					select {
					case <-timeout.Done():
						glog.Warning("remotetrack: timeout reading rtp , track id: ", t.track.ID())
						return
					case <-readDoneChan:
						return
					}
				}()

				t.onRead(rtp)

				readDoneChan <- true

				go t.client.updateReceiverStats(t)

			}
		}
	}()
}

func (t *RemoteTrack) updateStats(s stats.Stats) {
	// update the stats if the last update equal or more than 1 second
	latestUpdated := t.latestUpdatedTS.Load()
	if time.Since(time.Unix(0, int64(latestUpdated))).Seconds() <= 1 {
		return
	}

	if latestUpdated == 0 {
		t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))
		return
	}

	t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))

	deltaTime := time.Since(time.Unix(0, int64(latestUpdated)))
	current := t.currentBytesReceived.Load()
	t.previousBytesReceived.Store(current)
	t.currentBytesReceived.Store(s.BytesReceived)

	t.bitrate.Store(uint32((s.BytesReceived-current)*8) / uint32(deltaTime.Seconds()))

}

func (t *RemoteTrack) Track() *webrtc.TrackRemote {
	return t.track
}

func (t *RemoteTrack) GetCurrentBitrate() uint32 {
	return t.bitrate.Load()
}
