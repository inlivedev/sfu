package sfu

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"sync/atomic"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/networkmonitor"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type remoteTrack struct {
	context               context.Context
	mu                    sync.RWMutex
	track                 IRemoteTrack
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
	looping               bool
	monitor               *networkmonitor.NetworkMonitor
}

func newRemoteTrack(ctx context.Context, useBuffer bool, track IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), statsGetter stats.Getter, onStatsUpdated func(*stats.Stats), onRead func(*rtp.Packet), onNetworkConditionChanged func(networkmonitor.NetworkConditionType)) *remoteTrack {
	localctx, cancel := context.WithCancel(ctx)

	go func() {
		defer cancel()
		<-localctx.Done()
	}()

	rt := &remoteTrack{
		context:               localctx,
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
		monitor:               networkmonitor.Default(),
	}

	if useBuffer && track.Kind() == webrtc.RTPCodecTypeVideo {
		rt.packetBuffers = newPacketBuffers(localctx, minWait, maxWait, true)
	}

	rt.monitor.OnNetworkConditionChanged(onNetworkConditionChanged)

	if pliInterval > 0 {
		rt.enableIntervalPLI(pliInterval)
	}

	go rt.readRTP()

	if useBuffer && track.Kind() == webrtc.RTPCodecTypeVideo {
		go rt.loop()
	}

	return rt
}

func (t *remoteTrack) Buffered() bool {
	return t.packetBuffers != nil
}

func (t *remoteTrack) Context() context.Context {
	return t.context
}

func (t *remoteTrack) readRTP() {
	readCtx, cancel := context.WithCancel(t.context)
	defer cancel()

	for {
		select {
		case <-readCtx.Done():
			return
		default:
			if err := t.track.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
				glog.Error("remotetrack: set read deadline error: ", err)
				return
			}
			buffer, ok := rtppool.GetPacketManager().PayloadPool.Get().(*[]byte)
			if !ok {
				rtppool.GetPacketManager().PayloadPool.Put(buffer)
				return
			}

			n, _, readErr := t.track.Read(*buffer)
			if readErr == io.EOF {
				glog.Info("remotetrack: track ended: ", t.track.ID())
				rtppool.GetPacketManager().PayloadPool.Put(buffer)
				return
			}

			// could be read deadline reached
			if n == 0 {
				rtppool.GetPacketManager().PayloadPool.Put(buffer)
				continue
			}

			p := rtppool.GetPacketAllocationFromPool()

			if err := t.unmarshal((*buffer)[:n], p); err != nil {
				rtppool.GetPacketManager().PayloadPool.Put(buffer)
				rtppool.ResetPacketPoolAllocation(p)
				continue
			}

			if !t.IsRelay() {
				go t.updateStats()
			}

			if t.Buffered() && t.Track().Kind() == webrtc.RTPCodecTypeVideo {
				retainablePacket := rtppool.NewPacket(&p.Header, p.Payload)
				_ = t.packetBuffers.Add(retainablePacket)

			} else {
				t.onRead(p)
			}

			t.monitor.Add(p.SequenceNumber)

			rtppool.GetPacketManager().PayloadPool.Put(buffer)
			rtppool.ResetPacketPoolAllocation(p)
		}
	}
}

func (t *remoteTrack) unmarshal(buf []byte, p *rtp.Packet) error {
	n, err := p.Header.Unmarshal(buf)
	if err != nil {
		return err
	}

	end := len(buf)
	if p.Header.Padding {
		p.PaddingSize = buf[end-1]
		end -= int(p.PaddingSize)
	}
	if end < n {
		return errors.New("remote track buffer too short")
	}

	p.Payload = buf[n:end]

	return nil
}

func (t *remoteTrack) loop() {
	if t.looping {
		return
	}

	ctx, cancel := context.WithCancel(t.context)
	defer cancel()

	t.mu.Lock()
	t.looping = true
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.looping = false
		t.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			t.packetBuffers.WaitAvailablePacket()
			t.mu.RLock()
			for orderedPkt := t.packetBuffers.Pop(); orderedPkt != nil; orderedPkt = t.packetBuffers.Pop() {
				// make sure the we're passing a new packet to the onRead callback

				copyPkt := rtppool.GetPacketAllocationFromPool()

				copyPkt.Header = *orderedPkt.Header()

				copyPkt.Payload = orderedPkt.Payload()

				t.onRead(copyPkt)

				rtppool.ResetPacketPoolAllocation(copyPkt)

				orderedPkt.Release()
			}

			t.mu.RUnlock()

		}
	}

}

func (t *remoteTrack) Flush() {
	pkts := t.packetBuffers.Flush()
	for _, pkt := range pkts {
		copyPkt := rtppool.GetPacketAllocationFromPool()

		copyPkt.Header = *pkt.Header()

		copyPkt.Payload = pkt.Payload()

		t.onRead(copyPkt)

		rtppool.ResetPacketPoolAllocation(copyPkt)

		pkt.Release()
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
	t.mu.Lock()
	defer t.mu.Unlock()

	// return if there is a pending PLI request
	maxGapSeconds := 250 * time.Millisecond
	requestGap := time.Since(t.lastPLIRequestTime)

	if requestGap < maxGapSeconds {
		return // ignore PLI request
	}

	t.lastPLIRequestTime = time.Now()

	go t.onPLI()
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
