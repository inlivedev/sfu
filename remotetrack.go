package sfu

import (
	"context"
	"io"
	"sync"
	"time"

	"sync/atomic"

	"github.com/golang/glog"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type remoteTrack struct {
	client                *Client
	context               context.Context
	cancel                context.CancelFunc
	mu                    sync.Mutex
	track                 *webrtc.TrackRemote
	receiver              *webrtc.RTPReceiver
	onRead                func(*rtp.Packet)
	bitrate               *atomic.Uint32
	previousBytesReceived *atomic.Uint64
	currentBytesReceived  *atomic.Uint64
	latestUpdatedTS       *atomic.Uint64
	lastPLIRequestTime    time.Time
	onEndedCallbacks      []func()
	stats                 stats.Stats
}

func newRemoteTrack(client *Client, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, onRead func(*rtp.Packet)) *remoteTrack {
	ctx, cancel := context.WithCancel(client.context)
	rt := &remoteTrack{
		context:               ctx,
		cancel:                cancel,
		client:                client,
		mu:                    sync.Mutex{},
		track:                 track,
		receiver:              receiver,
		onRead:                onRead,
		bitrate:               &atomic.Uint32{},
		previousBytesReceived: &atomic.Uint64{},
		currentBytesReceived:  &atomic.Uint64{},
		latestUpdatedTS:       &atomic.Uint64{},
		onEndedCallbacks:      make([]func(), 0),
		stats:                 stats.Stats{},
	}

	rt.enableIntervalPLI(client.sfu.PLIInterval())

	rt.readRTP()

	return rt
}

func (t *remoteTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *remoteTrack) onEnded() {
	for _, f := range t.onEndedCallbacks {
		f()
	}
}

func (t *remoteTrack) readRTP() {
	go func() {
		defer t.cancel()

		for {
			select {
			case <-t.context.Done():

				return
			default:
				rtp, _, readErr := t.track.ReadRTP()
				if readErr == io.EOF {
					t.onEnded()

					t.client.stats.removeReceiverStats(t.track.ID())

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

func (t *remoteTrack) updateStats() {
	s := t.stats
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

func (t *remoteTrack) Track() *webrtc.TrackRemote {
	return t.track
}

func (t *remoteTrack) GetCurrentBitrate() uint32 {
	return t.bitrate.Load()
}

func (t *remoteTrack) receiverStats() stats.Stats {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.stats
}

func (t *remoteTrack) setReceiverStats(s stats.Stats) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats = s

	t.updateStats()
}

func (t *remoteTrack) sendPLI() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	maxGapSeconds := 250 * time.Millisecond
	requestGap := time.Since(t.lastPLIRequestTime)

	if requestGap < maxGapSeconds {
		return nil
	}

	t.lastPLIRequestTime = time.Now()

	return t.client.peerConnection.PC().WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(t.track.SSRC())},
	})
}

func (t *remoteTrack) enableIntervalPLI(interval time.Duration) {
	go func() {
		ctx, cancel := context.WithCancel(t.context)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := t.sendPLI(); err != nil {
					glog.Error("remotetrack: error sending PLI: ", err.Error())
				}
			}
		}
	}()
}
