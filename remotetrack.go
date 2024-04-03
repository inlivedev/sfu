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

	onRead             func(*rtp.Packet)
	onPLI              func()
	latestUpdatedTS    *atomic.Uint64
	lastPLIRequestTime time.Time
	onEndedCallbacks   []func()
	statsGetter        stats.Getter
	onStatsUpdated     func(*stats.Stats)
	packetBuffers      *packetBuffers
}

func newRemoteTrack(ctx context.Context, track IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), statsGetter stats.Getter, onStatsUpdated func(*stats.Stats), onRead func(*rtp.Packet)) *remoteTrack {
	localctx, cancel := context.WithCancel(ctx)

	rt := &remoteTrack{
		context:          localctx,
		cancel:           cancel,
		mu:               sync.RWMutex{},
		track:            track,
		latestUpdatedTS:  &atomic.Uint64{},
		onEndedCallbacks: make([]func(), 0),
		statsGetter:      statsGetter,
		onStatsUpdated:   onStatsUpdated,
		onPLI:            onPLI,
		onRead:           onRead,
		packetBuffers:    newPacketBuffers(localctx, minWait, maxWait, true),
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

			// could be read deadline reached
			if p == nil {
				continue
			}

			if !t.IsRelay() {
				go t.updateStats()
			}

			if t.Track().Kind() == webrtc.RTPCodecTypeVideo {
				// video needs to be reordered
				_ = t.packetBuffers.Add(p)
			} else {
				// audio doesn't need to be reordered
				t.onRead(p)
			}
		}
	}
}

func (t *remoteTrack) loop() {
	ctx, cancel := context.WithCancel(t.context)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			t.packetBuffers.WaitAvailablePacket()

			for orderedPkt := t.packetBuffers.Pop(); orderedPkt != nil; orderedPkt = t.packetBuffers.Pop() {
				// make sure the we're passing a new packet to the onRead callback
				copyPkt := rtppool.GetPacketAllocationFromPool()

				copyPkt.Header = *orderedPkt.Header()

				copyPkt.Payload = orderedPkt.Payload()

				t.onRead(copyPkt)

				rtppool.ResetPacketPoolAllocation(copyPkt)

				orderedPkt.Release()
			}

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

	if t.onStatsUpdated != nil {
		t.onStatsUpdated(s)
	}
}

func (t *remoteTrack) Track() IRemoteTrack {
	return t.track
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

func (t *remoteTrack) Buffer() *packetBuffers {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.packetBuffers
}
