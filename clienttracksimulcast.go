package sfu

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type simulcastClientTrack struct {
	id                      string
	mu                      sync.RWMutex
	client                  *Client
	context                 context.Context
	cancel                  context.CancelFunc
	kind                    webrtc.RTPCodecType
	mimeType                string
	localTrack              *webrtc.TrackLocalStaticRTP
	remoteTrack             *SimulcastTrack
	lastBlankSequenceNumber *atomic.Uint32
	sequenceNumber          *atomic.Uint32
	lastQuality             *atomic.Uint32
	paddingTS               *atomic.Uint32
	maxQuality              *atomic.Uint32
	lastTimestamp           *atomic.Uint32
	isScreen                *atomic.Bool
	isEnded                 *atomic.Bool
	onTrackEndedCallbacks   []func()
}

func newSimulcastClientTrack(c *Client, t *SimulcastTrack) *simulcastClientTrack {
	ctx, cancel := context.WithCancel(t.Context())
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.base.codec.RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	isScreen := &atomic.Bool{}
	isScreen.Store(t.IsScreen())

	lastQuality := &atomic.Uint32{}

	sequenceNumber := &atomic.Uint32{}

	lastTimestamp := &atomic.Uint32{}

	ct := &simulcastClientTrack{
		mu:                      sync.RWMutex{},
		id:                      GenerateID(16),
		context:                 ctx,
		cancel:                  cancel,
		kind:                    t.base.kind,
		mimeType:                t.base.codec.MimeType,
		client:                  c,
		localTrack:              track,
		remoteTrack:             t,
		sequenceNumber:          sequenceNumber,
		lastQuality:             lastQuality,
		paddingTS:               &atomic.Uint32{},
		maxQuality:              &atomic.Uint32{},
		lastBlankSequenceNumber: &atomic.Uint32{},
		lastTimestamp:           lastTimestamp,
		isScreen:                isScreen,
		isEnded:                 &atomic.Bool{},
		onTrackEndedCallbacks:   make([]func(), 0),
	}

	ct.SetMaxQuality(QualityHigh)

	ct.remoteTrack.sendPLI()

	return ct
}

func (t *simulcastClientTrack) Client() *Client {
	return t.client
}

func (t *simulcastClientTrack) Context() context.Context {
	return t.context
}

func (t *simulcastClientTrack) isFirstKeyframePacket(p *rtp.Packet) bool {
	isKeyframe := IsKeyframe(t.mimeType, p)

	return isKeyframe && t.lastTimestamp.Load() != p.Timestamp
}

func (t *simulcastClientTrack) send(p *rtp.Packet, quality QualityLevel) {
	t.lastTimestamp.Store(p.Timestamp)

	t.rewritePacket(p, quality)

	// glog.Info("track: ", t.id, " send packet with quality ", quality, " and sequence number ", p.SequenceNumber)

	t.writeRTP(p)
}

func (t *simulcastClientTrack) writeRTP(p *rtp.Packet) {
	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("track: error on write rtp", err)
	}
}

func (t *simulcastClientTrack) push(p *rtp.Packet, quality QualityLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	currentQuality := t.LastQuality()

	targetQuality := t.getQuality()

	if !t.client.bitrateController.exists(t.ID()) {
		// do nothing if the bitrate claim is not exist
		return
	}

	isKeyframe := IsKeyframe(t.mimeType, p)

	// check if it's a first packet to send
	if currentQuality == QualityNone && t.sequenceNumber.Load() == 0 {
		// we try to send the low quality first	if the track is active and fallback to upper quality if not
		if t.remoteTrack.getRemoteTrack(QualityLow) != nil && quality == QualityLow {
			t.lastQuality.Store(uint32(QualityLow))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI()
		} else if t.remoteTrack.getRemoteTrack(QualityMid) != nil && quality == QualityMid {
			t.lastQuality.Store(uint32(QualityMid))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI()
		} else if t.remoteTrack.getRemoteTrack(QualityHigh) != nil && quality == QualityHigh {
			t.lastQuality.Store(uint32(QualityHigh))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI()
		}

		t.remoteTrack.onRemoteTrackAdded(func(remote *remoteTrack) {
			t.remoteTrack.sendPLI()
		})
	} else if isKeyframe && quality == targetQuality && t.lastQuality.Load() != uint32(targetQuality) {
		glog.Info("track: ", t.id, " keyframe ", isKeyframe, " change quality from ", t.lastQuality.Load(), " to ", targetQuality)
		currentQuality = targetQuality
		t.lastQuality.Store(uint32(currentQuality))

	} else if quality == targetQuality && !isKeyframe && t.lastQuality.Load() != uint32(targetQuality) {
		glog.Info("track: ", t.id, " keyframe ", isKeyframe, " send keyframe and sequence number ", p.SequenceNumber)
		t.remoteTrack.sendPLI()
	}

	if currentQuality == quality {
		t.send(p, quality)
	}
}

func (t *simulcastClientTrack) GetRemoteTrack() *remoteTrack {
	lastQuality := Uint32ToQualityLevel(t.lastQuality.Load())
	// lastQuality := t.lastQuality
	switch lastQuality {
	case QualityHigh:
		return t.remoteTrack.remoteTrackHigh
	case QualityMid:
		return t.remoteTrack.remoteTrackMid
	case QualityLow:
		return t.remoteTrack.remoteTrackLow
	default:
		if t.remoteTrack.isTrackActive(QualityHigh) {
			return t.remoteTrack.remoteTrackHigh
		}

		if t.remoteTrack.isTrackActive(QualityMid) {
			return t.remoteTrack.remoteTrackMid
		}

		if t.remoteTrack.isTrackActive(QualityLow) {
			return t.remoteTrack.remoteTrackLow
		}
	}

	return nil
}

func (t *simulcastClientTrack) getCurrentBitrate() uint32 {
	currentTrack := t.GetRemoteTrack()
	if currentTrack == nil {
		return 0
	}

	return currentTrack.GetCurrentBitrate()
}

func (t *simulcastClientTrack) ID() string {
	return t.id
}

func (t *simulcastClientTrack) Kind() webrtc.RTPCodecType {
	return t.kind
}

func (t *simulcastClientTrack) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *simulcastClientTrack) IsScreen() bool {
	return t.isScreen.Load()
}

func (t *simulcastClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *simulcastClientTrack) LastQuality() QualityLevel {
	return Uint32ToQualityLevel(t.lastQuality.Load())
}

func (t *simulcastClientTrack) OnTrackEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, callback)
}

func (t *simulcastClientTrack) onTrackEnded() {
	if t.isEnded.Load() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.onTrackEndedCallbacks {
		callback()
	}

	t.isEnded.Store(true)
}

func (t *simulcastClientTrack) SetMaxQuality(quality QualityLevel) {
	t.maxQuality.Store(uint32(quality))
	t.remoteTrack.sendPLI()
}

func (t *simulcastClientTrack) MaxQuality() QualityLevel {
	return Uint32ToQualityLevel(t.maxQuality.Load())
}

func (t *simulcastClientTrack) IsSimulcast() bool {
	return true
}

func (t *simulcastClientTrack) IsScaleable() bool {
	return false
}

func (t *simulcastClientTrack) rewritePacket(p *rtp.Packet, quality QualityLevel) {
	t.remoteTrack.mu.RLock()
	defer t.remoteTrack.mu.RUnlock()
	// make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track
	sequenceDelta := uint16(0)
	// credit to https://github.com/k0nserv for helping me with this on Pion Slack channel
	switch quality {
	case QualityHigh:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackHighBaseTS) - t.remoteTrack.remoteTrackHighBaseTS)
		sequenceDelta = t.remoteTrack.highSequence - t.remoteTrack.lastHighSequence
	case QualityMid:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackMidBaseTS) - t.remoteTrack.remoteTrackMidBaseTS)
		sequenceDelta = t.remoteTrack.midSequence - t.remoteTrack.lastMidSequence
	case QualityLow:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackLowBaseTS) - t.remoteTrack.remoteTrackLowBaseTS)
		sequenceDelta = t.remoteTrack.lowSequence - t.remoteTrack.lastLowSequence
	}

	t.sequenceNumber.Add(uint32(sequenceDelta))
	p.SequenceNumber = uint16(t.sequenceNumber.Load())
}

func (t *simulcastClientTrack) RequestPLI() {
	t.remoteTrack.sendPLI()
}

func (t *simulcastClientTrack) getQuality() QualityLevel {
	track := t.remoteTrack

	claim := t.client.bitrateController.GetClaim(t.ID())

	quality := min(claim.quality, t.MaxQuality(), Uint32ToQualityLevel(t.client.quality.Load()))

	if quality != QualityNone && !track.isTrackActive(quality) {
		if quality != QualityLow && track.isTrackActive(QualityLow) {
			return QualityLow
		}

		if quality != QualityMid && track.isTrackActive(QualityMid) {
			return QualityMid
		}

		if quality != QualityHigh && track.isTrackActive(QualityHigh) {
			return QualityHigh
		}
	}

	return quality
}
