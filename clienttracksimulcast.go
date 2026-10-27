package sfu

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inlivedev/sfu/pkg/packetmap"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type simulcastClientTrack struct {
	id                      string
	streamid                string
	mu                      sync.RWMutex
	client                  *Client
	context                 context.Context
	kind                    webrtc.RTPCodecType
	mimeType                string
	localTrack              *webrtc.TrackLocalStaticRTP
	remoteTrack             *SimulcastTrack
	baseTrack               *baseTrack
	lastBlankSequenceNumber *atomic.Uint32
	writeMu                 sync.Mutex
	rewriter                *simulcastRewriter
	lastQuality             *atomic.Uint32
	paddingTS               *atomic.Uint32
	maxQuality              *atomic.Uint32
	lastTimestamp           *atomic.Uint32
	isScreen                *atomic.Bool
	isEnded                 *atomic.Bool
	packetmapHigh           *packetmap.Map
	packetmapMid            *packetmap.Map
	packetmapLow            *packetmap.Map
	onTrackEndedCallbacks   []func()
}

func newSimulcastClientTrack(c *Client, t *SimulcastTrack) *simulcastClientTrack {
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.base.codec.RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	isScreen := &atomic.Bool{}
	isScreen.Store(t.IsScreen())

	lastQuality := &atomic.Uint32{}

	lastTimestamp := &atomic.Uint32{}

	ctx, cancel := context.WithCancel(t.context)

	ct := &simulcastClientTrack{
		mu:                      sync.RWMutex{},
		id:                      track.ID(),
		streamid:                track.StreamID(),
		context:                 ctx,
		kind:                    track.Kind(),
		mimeType:                track.Codec().MimeType,
		client:                  c,
		localTrack:              track,
		remoteTrack:             t,
		baseTrack:               t.base,
		rewriter:                newSimulcastRewriter(t.base.codec.ClockRate),
		lastQuality:             lastQuality,
		paddingTS:               &atomic.Uint32{},
		maxQuality:              &atomic.Uint32{},
		lastBlankSequenceNumber: &atomic.Uint32{},
		lastTimestamp:           lastTimestamp,
		isScreen:                isScreen,
		isEnded:                 &atomic.Bool{},
		onTrackEndedCallbacks:   make([]func(), 0),
		packetmapHigh:           &packetmap.Map{},
		packetmapMid:            &packetmap.Map{},
		packetmapLow:            &packetmap.Map{},
	}

	ct.SetMaxQuality(QualityHigh)

	ct.remoteTrack.sendPLI()

	t.OnEnded(func() {
		ct.onEnded()
		cancel()
	})

	return ct
}

func (t *simulcastClientTrack) Client() *Client {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.client
}

func (t *simulcastClientTrack) Context() context.Context {
	return t.context
}

func (t *simulcastClientTrack) isFirstKeyframePacket(p *rtp.Packet) bool {
	isKeyframe := IsKeyframe(t.mimeType, p.Payload)

	return isKeyframe && t.lastTimestamp.Load() != p.Timestamp
}

func (t *simulcastClientTrack) send(p *rtp.Packet, quality QualityLevel) {
	// each layer is pushed from its own read goroutine, so rewriting and writing
	// must be serialized to keep the outgoing sequence numbers in order
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	// the quality may have switched since push checked it
	if t.LastQuality() != quality {
		return
	}

	t.lastTimestamp.Store(p.Timestamp)

	if !t.rewriter.rewrite(p, quality, time.Now()) {
		return
	}

	t.writeRTP(p)
}

func (t *simulcastClientTrack) writeRTP(p *rtp.Packet) {
	if err := t.localTrack.WriteRTP(p); err != nil {
		t.client.log.Errorf("track: error on write rtp", err)
	}
}

func (t *simulcastClientTrack) push(p *rtp.Packet, quality QualityLevel) {
	isKeyframe := IsKeyframe(t.mimeType, p.Payload)

	currentQuality := t.LastQuality()

	targetQuality := t.getQuality()

	if targetQuality == QualityNone {
		// TODO: figure out what to do if the target quality is none
		// probably we should send a blank frame
		targetQuality = QualityLow
	}

	if !t.client.bitrateController.Exist(t.ID()) {
		// do nothing if the bitrate claim is not exist
		return
	}

	var canSwitch bool

	switch quality {
	case QualityHigh:
		canSwitch, _, _ = t.packetmapHigh.Map(p.SequenceNumber, 0)
	case QualityMid:
		canSwitch, _, _ = t.packetmapMid.Map(p.SequenceNumber, 0)
	case QualityLow:
		canSwitch, _, _ = t.packetmapLow.Map(p.SequenceNumber, 0)
	}

	// check if it's a first packet to send
	if currentQuality == QualityNone && !t.rewriter.hasStarted() {
		// we try to send the low quality first	if the track is active and fallback to upper quality if not
		if t.remoteTrack.GetRemoteTrack(QualityLow) != nil && quality == QualityLow {
			t.lastQuality.Store(uint32(QualityLow))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI()
		} else if t.remoteTrack.GetRemoteTrack(QualityMid) != nil && quality == QualityMid {
			t.lastQuality.Store(uint32(QualityMid))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI()
		} else if t.remoteTrack.GetRemoteTrack(QualityHigh) != nil && quality == QualityHigh {
			t.lastQuality.Store(uint32(QualityHigh))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI()
		}

		t.remoteTrack.onRemoteTrackAdded(func(remote *remoteTrack) {
			t.remoteTrack.sendPLI()
		})
	} else if isKeyframe && canSwitch && quality == targetQuality && currentQuality != targetQuality {
		// change quality to target quality if it's a keyframe
		t.client.log.Tracef("track: %s keyframe %v change quality from %d to %d ", t.id, isKeyframe, t.lastQuality.Load(), targetQuality)
		currentQuality = targetQuality
		t.lastQuality.Store(uint32(currentQuality))

	} else if quality == targetQuality && !isKeyframe && currentQuality != targetQuality {
		// request PLI to allow us switch quality to target quality
		t.client.log.Tracef("track: %s keyframe %v send keyframe and sequence number %d and can switch %v ", t.id, isKeyframe, p.SequenceNumber, canSwitch)
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

func (t *simulcastClientTrack) ID() string {
	return t.id
}

func (t *simulcastClientTrack) StreamID() string {
	return t.streamid
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
	quality := Uint32ToQualityLevel(t.lastQuality.Load())

	track := t.remoteTrack

	if quality == QualityHigh && track.isTrackActive(QualityHigh) {
		return QualityHigh
	} else if quality == QualityMid && track.isTrackActive(QualityMid) {
		return QualityMid
	}
	return QualityLow
}

func (t *simulcastClientTrack) OnEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, callback)
}

func (t *simulcastClientTrack) onEnded() {
	if t.isEnded.Load() {
		return
	}

	for _, callback := range t.onTrackEndedCallbacks {
		callback()
	}

	t.isEnded.Store(true)
}

func (t *simulcastClientTrack) SetMaxQuality(quality QualityLevel) {
	t.maxQuality.Store(uint32(quality))
	claim := t.Client().bitrateController.GetClaim(t.ID())
	if claim != nil {
		if claim.Quality() > quality && quality != QualityNone {
			claim.SetQuality(quality)
		}
	}

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

// simulcastRewriter maps packets from the simulcast layers onto one outgoing
// stream. The sequence number and timestamp offsets are fixed while a layer is
// forwarded and re-anchored on every layer switch, so the outgoing stream stays
// contiguous and monotonic. Retransmissions of the current layer keep their
// place, and packets from before the switch are dropped because their outgoing
// sequence numbers belong to the previous layer. It is not safe for concurrent use.
type simulcastRewriter struct {
	clockRate uint32
	started   bool
	layer     QualityLevel
	anchorSeq uint16
	seqOffset uint16
	tsOffset  uint32
	lastSeq   uint16
	lastTS    uint32
	lastTime  time.Time
}

func newSimulcastRewriter(clockRate uint32) *simulcastRewriter {
	if clockRate == 0 {
		clockRate = 90000
	}

	return &simulcastRewriter{clockRate: clockRate}
}

func (r *simulcastRewriter) hasStarted() bool {
	return r.started
}

// rewrite sets the outgoing sequence number and timestamp of p, a packet of the
// given layer. It returns false if the packet must be dropped.
func (r *simulcastRewriter) rewrite(p *rtp.Packet, layer QualityLevel, now time.Time) bool {
	if !r.started || layer != r.layer {
		r.anchor(p, layer, now)
	} else if int16(p.SequenceNumber-r.anchorSeq) < 0 {
		// sent before we switched to this layer
		return false
	}

	seq := p.SequenceNumber + r.seqOffset
	ts := p.Timestamp + r.tsOffset

	if int16(seq-r.lastSeq) > 0 {
		r.lastSeq = seq
		r.lastTS = ts
		r.lastTime = now
	}

	p.SequenceNumber = seq
	p.Timestamp = ts

	return true
}

func (r *simulcastRewriter) anchor(p *rtp.Packet, layer QualityLevel, now time.Time) {
	r.layer = layer
	r.anchorSeq = p.SequenceNumber

	if !r.started {
		r.started = true
		r.seqOffset = 0
		r.tsOffset = 0
		r.lastSeq = p.SequenceNumber - 1
		r.lastTS = p.Timestamp
		r.lastTime = now

		return
	}

	// advance the timestamp by the wall clock time since the last packet, so the
	// new layer's frames continue after the previous layer's frames
	elapsed := uint32(max(now.Sub(r.lastTime), 0).Milliseconds()) * (r.clockRate / 1000)
	if elapsed == 0 {
		elapsed = 1
	}

	r.seqOffset = r.lastSeq + 1 - p.SequenceNumber
	r.tsOffset = r.lastTS + elapsed - p.Timestamp
}

func (t *simulcastClientTrack) RequestPLI() {
	t.remoteTrack.sendPLI()
}

func (t *simulcastClientTrack) getQuality() QualityLevel {
	track := t.remoteTrack

	claim := t.Client().bitrateController.GetClaim(t.ID())

	if claim == nil {
		return QualityNone
	}

	quality := min(claim.Quality(), t.MaxQuality(), Uint32ToQualityLevel(t.client.quality.Load()))

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

func (t *simulcastClientTrack) ReceiveBitrate() uint32 {
	total := uint32(0)

	for _, quality := range []QualityLevel{QualityHigh, QualityMid, QualityLow} {
		total += t.ReceiveBitrateAtQuality(quality)
	}

	return total
}

func (t *simulcastClientTrack) SendBitrate() uint32 {
	bitrate, err := t.client.stats.GetSenderBitrate(t.ID())
	if err != nil {
		t.client.log.Errorf("clienttrack: error on get sender", err)
		return 0
	}

	return bitrate
}

func (t *simulcastClientTrack) ReceiveBitrateAtQuality(quality QualityLevel) uint32 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var remoteTrack *remoteTrack

	switch quality {
	case QualityHigh:
		remoteTrack = t.remoteTrack.remoteTrackHigh
	case QualityHighMid:
		remoteTrack = t.remoteTrack.remoteTrackHigh
	case QualityHighLow:
		remoteTrack = t.remoteTrack.remoteTrackHigh
	case QualityMid:
		remoteTrack = t.remoteTrack.remoteTrackMid
	case QualityMidMid:
		remoteTrack = t.remoteTrack.remoteTrackMid
	case QualityMidLow:
		remoteTrack = t.remoteTrack.remoteTrackMid
	case QualityLow:
		remoteTrack = t.remoteTrack.remoteTrackLow
	case QualityLowMid:
		remoteTrack = t.remoteTrack.remoteTrackLow
	case QualityLowLow:
		remoteTrack = t.remoteTrack.remoteTrackLow
	}

	if remoteTrack == nil {
		return 0
	}

	bitrate, err := t.baseTrack.client.stats.GetReceiverBitrate(remoteTrack.track.ID(), remoteTrack.track.RID())
	if err != nil {
		t.client.log.Errorf("clienttrack: error on get receiver", err)
		return 0
	}

	switch quality {
	case QualityHighMid, QualityMidMid, QualityLowMid:
		return bitrate / 2
	case QualityHighLow, QualityMidLow, QualityLowLow:
		return bitrate / 4
	default:
		return bitrate
	}
}

func (t *simulcastClientTrack) Quality() QualityLevel {
	return t.getQuality()
}

func (t *simulcastClientTrack) MimeType() string {
	return t.mimeType
}
