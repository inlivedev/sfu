package sfu

import (
	"context"
	"io"
	"sync"
	"time"

	"sync/atomic"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type remoteTrack struct {
	context context.Context
	cancel  context.CancelFunc
	mu      sync.RWMutex
	track   IRemoteTrack

	onRead                func(*rtp.Packet)
	onPLI                 func()
	bitrate               *atomic.Uint32
	previousBytesReceived *atomic.Uint64
	currentBytesReceived  *atomic.Uint64
	latestUpdatedTS       *atomic.Uint64
	lastPLIRequestTime    time.Time
	onEndedCallbacks      []func()
	statsGetter           stats.Getter
	onStatsUpdated        func(*stats.Stats)
	packetBuffers         *packetBuffers
}

func newRemoteTrack(ctx context.Context, track IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), statsGetter stats.Getter, onStatsUpdated func(*stats.Stats), onRead func(*rtp.Packet)) *remoteTrack {
	localctx, cancel := context.WithCancel(ctx)

	rt := &remoteTrack{
		context:               localctx,
		cancel:                cancel,
		mu:                    sync.RWMutex{},
		track:                 track,
		bitrate:               &atomic.Uint32{},
		previousBytesReceived: &atomic.Uint64{},
		currentBytesReceived:  &atomic.Uint64{},
		latestUpdatedTS:       &atomic.Uint64{},
		onEndedCallbacks:      make([]func(), 0),
		statsGetter:           statsGetter,
		onStatsUpdated:        onStatsUpdated,
		onPLI:                 onPLI,
		onRead:                onRead,
		packetBuffers:         newPacketBuffers(minWait, maxWait),
	}

	if pliInterval > 0 {
		rt.enableIntervalPLI(pliInterval)
	}

	go rt.readRTP()

	if rt.Track().Kind() == webrtc.RTPCodecTypeVideo {
		go rt.loop()
	}

	return rt
}

func (t *remoteTrack) Context() context.Context {
	return t.context
}

func (t *remoteTrack) readRTP() {

	defer t.cancel()
	for {
		select {
		case <-t.context.Done():
			return
		default:
			if err := t.track.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
				glog.Error("remotetrack: set read deadline error: ", err)
				return
			}

			p, _, readErr := t.track.ReadRTP()
			if readErr == io.EOF {
				glog.Info("remotetrack: track ended: ", t.track.ID())
				return
			}

			if !t.IsRelay() {
				go t.updateStats()
			}

			if t.Track().Kind() == webrtc.RTPCodecTypeVideo {
				// video needs to be reordered
				if p != nil {
					packet := rtppool.RTPPacketPool.GetPacketAllocationFromPool()
					packet = p
					_ = t.packetBuffers.Add(packet)

				}
			} else {
				// audio doesn't need to be reordered
				if p != nil {
					t.onRead(p)
				}
			}
		}
	}
}

func (t *remoteTrack) loop() {
	ctx, cancel := context.WithCancel(t.context)
	defer cancel()

	waitTimeMs := 1
	lastAdjusted := time.Now()
	nillCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
			orderedPkt := t.packetBuffers.Pop()
			if orderedPkt == nil {
				if nillCount > 5 {
					waitTimeMs++
					nillCount = 0
					lastAdjusted = time.Now()
					glog.Info("remotetrack: ordered packet is nil, increasing wait time to ", waitTimeMs, "ms")
				}

				time.Sleep(time.Duration(waitTimeMs) * time.Millisecond)
				continue
			}

			if time.Since(lastAdjusted).Seconds() > 30 && waitTimeMs > 1 {
				waitTimeMs--
				glog.Info("remotetrack: packet pop quite stable, decreasing wait time to ", waitTimeMs, "ms")
			}

			t.onRead(orderedPkt)
			rtppool.RTPPacketPool.ResetPacketPoolAllocation(orderedPkt)
		}
	}

}

func (t *remoteTrack) updateStats() {
	s := t.statsGetter.Get(uint32(t.track.SSRC()))
	if s == nil {
		glog.Warning("remotetrack: stats not found for track: ", t.track.SSRC())
		return
	}

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

	if t.onStatsUpdated != nil {
		t.onStatsUpdated(s)
	}
}

func (t *remoteTrack) Track() IRemoteTrack {
	return t.track
}

func (t *remoteTrack) GetCurrentBitrate() uint32 {
	return t.bitrate.Load()
}

func (t *remoteTrack) sendPLI() {
	// return if there is a pending PLI request
	t.mu.Lock()

	maxGapSeconds := 250 * time.Millisecond
	requestGap := time.Since(t.lastPLIRequestTime)

	if requestGap < maxGapSeconds {
		t.mu.Unlock()
		return // ignore PLI request
	}

	t.lastPLIRequestTime = time.Now()
	t.mu.Unlock()

	t.onPLI()
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
				t.sendPLI()
			}
		}
	}()
}

func (t *remoteTrack) IsRelay() bool {
	_, ok := t.track.(*RelayTrack)
	return ok
}
