package sfu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/inlivedev/sfu/pkg/networkmonitor"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	TrackTypeMedia  = "media"
	TrackTypeScreen = "screen"
)

var (
	ErrTrackExists      = errors.New("client: error track already exists")
	ErrTrackIsNotExists = errors.New("client: error track is not exists")
)

type TrackType string

func (t TrackType) String() string {
	return string(t)
}

type baseTrack struct {
	id           string
	msid         string
	streamid     string
	client       *Client
	isProcessed  bool
	kind         webrtc.RTPCodecType
	codec        webrtc.RTPCodecParameters
	isScreen     *atomic.Bool // source of the track, can be media or screen
	clientTracks *clientTrackList
	pool         *rtppool.RTPPool
}

type ITrack interface {
	ID() string
	StreamID() string
	ClientID() string
	IsSimulcast() bool
	IsScaleable() bool
	IsProcessed() bool
	SetSourceType(TrackType)
	SourceType() TrackType
	SetAsProcessed()
	OnRead(func(interceptor.Attributes, *rtp.Packet, QualityLevel))
	IsScreen() bool
	IsRelay() bool
	Kind() webrtc.RTPCodecType
	MimeType() string
	TotalTracks() int
	Context() context.Context
	Relay(func(webrtc.SSRC, interceptor.Attributes, *rtp.Packet))
	PayloadType() webrtc.PayloadType
	OnEnded(func())
}

type Track struct {
	context          context.Context
	mu               sync.Mutex
	base             *baseTrack
	remoteTrack      *remoteTrack
	onEndedCallbacks []func()
	onReadCallbacks  []func(interceptor.Attributes, *rtp.Packet, QualityLevel)
}

type AudioTrack struct {
	*Track
	vad          *voiceactivedetector.VoiceDetector
	vadCallbacks []func([]voiceactivedetector.VoicePacketData)
}

func newTrack(ctx context.Context, client *Client, trackRemote IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), stats stats.Getter, onStatsUpdated func(*stats.Stats)) ITrack {
	ctList := newClientTrackList()
	pool := rtppool.New()
	baseTrack := &baseTrack{
		id:           trackRemote.ID(),
		isScreen:     &atomic.Bool{},
		msid:         trackRemote.Msid(),
		streamid:     trackRemote.StreamID(),
		client:       client,
		kind:         trackRemote.Kind(),
		codec:        trackRemote.Codec(),
		clientTracks: ctList,
		pool:         pool,
	}

	t := &Track{
		mu:               sync.Mutex{},
		base:             baseTrack,
		onReadCallbacks:  make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0),
		onEndedCallbacks: make([]func(), 0),
	}

	onRead := func(attrs interceptor.Attributes, p *rtp.Packet) {
		tracks := t.base.clientTracks.GetTracks()

		for _, track := range tracks {
			//nolint:ineffassign,staticcheck // packet is from the pool
			packet := pool.CopyPacket(p)

			track.push(packet, QualityHigh)

			pool.PutPacket(packet)
		}

		//nolint:ineffassign // this is required
		packet := pool.CopyPacket(p)

		t.onRead(attrs, packet, QualityHigh)

		pool.PutPacket(packet)
	}

	onNetworkConditionChanged := func(condition networkmonitor.NetworkConditionType) {
		client.onNetworkConditionChanged(condition)
	}

	t.remoteTrack = newRemoteTrack(ctx, client.log, client.options.ReorderPackets, trackRemote, minWait, maxWait, pliInterval, onPLI, stats, onStatsUpdated, onRead, pool, onNetworkConditionChanged)

	var cancel context.CancelFunc

	t.context, cancel = context.WithCancel(client.Context())

	t.remoteTrack.OnEnded(func() {
		cancel()
		t.onEnded()
	})

	if trackRemote.Kind() == webrtc.RTPCodecTypeAudio {
		ta := &AudioTrack{
			Track: t,
		}

		return ta
	}

	return t
}

func (t *Track) ClientID() string {
	return t.base.client.id
}

func (t *Track) Context() context.Context {
	return t.context
}

func (t *Track) createLocalTrack() *webrtc.TrackLocalStaticRTP {
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrack.track.Codec().RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	return track
}

func (t *Track) createOpusLocalTrack() *webrtc.TrackLocalStaticRTP {
	c := t.remoteTrack.track.Codec().RTPCodecCapability
	c.MimeType = webrtc.MimeTypeOpus
	c.SDPFmtpLine = "minptime=10;useinbandfec=1"
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(c, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	return track
}

func (t *Track) ID() string {
	return t.base.id
}

func (t *Track) StreamID() string {
	return t.base.streamid
}

func (t *Track) SSRC() webrtc.SSRC {
	return t.remoteTrack.track.SSRC()
}

func (t *AudioTrack) SetVAD(vad *voiceactivedetector.VoiceDetector) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.vad = vad
	vad.OnVoiceDetected(func(pkts []voiceactivedetector.VoicePacketData) {
		// send through datachannel
		t.onVoiceDetected(pkts)
	})
}

func (t *AudioTrack) onVoiceDetected(pkts []voiceactivedetector.VoicePacketData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.vadCallbacks {
		callback(pkts)
	}
}

func (t *AudioTrack) OnVoiceDetected(callback func(pkts []voiceactivedetector.VoicePacketData)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.vadCallbacks = append(t.vadCallbacks, callback)
}

func (t *AudioTrack) subscribe(c *Client) iClientTrack {
	var ct iClientTrack

	cta := newClientTrackAudio(c, t)

	if t.PayloadType() == 63 {
		t.base.client.log.Tracef("track: red enabled %v", c.receiveRED)

		// TODO: detect if client supports RED and it's audio then send RED encoded packets
		ct = newClientTrackRed(cta)
	} else {
		ct = cta
	}

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *Track) RemoteTrack() *remoteTrack {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.remoteTrack
}

func (t *Track) IsScreen() bool {
	return t.base.isScreen.Load()
}

func (t *Track) IsSimulcast() bool {
	return false
}

func (t *Track) IsScaleable() bool {
	return t.MimeType() == webrtc.MimeTypeVP9
}

func (t *Track) IsProcessed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.isProcessed
}

func (t *Track) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *Track) MimeType() string {
	return t.base.codec.MimeType
}

func (t *Track) SSRCHigh() webrtc.SSRC {
	return t.remoteTrack.Track().SSRC()
}

func (t *Track) SSRCMid() webrtc.SSRC {
	return t.remoteTrack.Track().SSRC()
}

func (t *Track) SSRCLow() webrtc.SSRC {
	return t.remoteTrack.Track().SSRC()
}

func (t *Track) TotalTracks() int {
	return 1
}

func (t *Track) subscribe(c *Client) iClientTrack {
	var ct iClientTrack

	if t.MimeType() == webrtc.MimeTypeVP9 {
		ct = newScaleableClientTrack(c, t)
	} else {
		ct = newClientTrack(c, t, t.IsScreen(), nil)
	}

	if t.Kind() == webrtc.RTPCodecTypeVideo {
		t.remoteTrack.SendPLI()
	}

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *Track) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *Track) SourceType() TrackType {
	if t.base.isScreen.Load() {
		return TrackTypeScreen
	}

	return TrackTypeMedia
}

func (t *Track) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *Track) OnRead(callback func(interceptor.Attributes, *rtp.Packet, QualityLevel)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onReadCallbacks = append(t.onReadCallbacks, callback)
}

func (t *Track) onRead(attrs interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
	callbacks := make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0)

	t.mu.Lock()
	callbacks = append(callbacks, t.onReadCallbacks...)
	t.mu.Unlock()

	for _, callback := range callbacks {
		copyPacket := t.base.pool.CopyPacket(p)
		callback(attrs, copyPacket, quality)
		t.base.pool.PutPacket(copyPacket)
	}
}

func (t *Track) Relay(f func(webrtc.SSRC, interceptor.Attributes, *rtp.Packet)) {
	t.OnRead(func(attrs interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
		f(t.SSRC(), attrs, p)
	})
}

func (t *Track) PayloadType() webrtc.PayloadType {
	return t.base.codec.PayloadType
}

func (t *Track) IsRelay() bool {
	return t.remoteTrack.IsRelay()
}

func (t *Track) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *Track) onEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, f := range t.onEndedCallbacks {
		f()
	}
}

type SimulcastTrack struct {
	context                     context.Context
	cancel                      context.CancelFunc
	mu                          sync.RWMutex
	base                        *baseTrack
	baseTS                      uint32
	onTrackCompleteCallbacks    []func()
	remoteTrackHigh             *remoteTrack
	remoteTrackHighBaseTS       uint32
	highSequence                uint16
	lastHighSequence            uint16
	remoteTrackMid              *remoteTrack
	remoteTrackMidBaseTS        uint32
	midSequence                 uint16
	lastMidSequence             uint16
	remoteTrackLow              *remoteTrack
	remoteTrackLowBaseTS        uint32
	lowSequence                 uint16
	lastLowSequence             uint16
	lastReadHighTS              *atomic.Int64
	lastReadMidTS               *atomic.Int64
	lastReadLowTS               *atomic.Int64
	lastHighKeyframeTS          *atomic.Int64
	lastMidKeyframeTS           *atomic.Int64
	lastLowKeyframeTS           *atomic.Int64
	onAddedRemoteTrackCallbacks []func(*remoteTrack)
	onReadCallbacks             []func(interceptor.Attributes, *rtp.Packet, QualityLevel)
	pliInterval                 time.Duration
	onNetworkConditionChanged   func(networkmonitor.NetworkConditionType)
	reordered                   bool
	onEndedCallbacks            []func()
}

func newSimulcastTrack(client *Client, track IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), stats stats.Getter, onStatsUpdated func(*stats.Stats)) ITrack {

	t := &SimulcastTrack{
		mu: sync.RWMutex{},
		base: &baseTrack{
			id:           track.ID(),
			isScreen:     &atomic.Bool{},
			msid:         track.Msid(),
			streamid:     track.StreamID(),
			client:       client,
			kind:         track.Kind(),
			codec:        track.Codec(),
			clientTracks: newClientTrackList(),
			pool:         rtppool.New(),
		},
		lastReadHighTS:              &atomic.Int64{},
		lastReadMidTS:               &atomic.Int64{},
		lastReadLowTS:               &atomic.Int64{},
		lastHighKeyframeTS:          &atomic.Int64{},
		lastMidKeyframeTS:           &atomic.Int64{},
		lastLowKeyframeTS:           &atomic.Int64{},
		onTrackCompleteCallbacks:    make([]func(), 0),
		onAddedRemoteTrackCallbacks: make([]func(*remoteTrack), 0),
		onReadCallbacks:             make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0),
		pliInterval:                 pliInterval,
		onNetworkConditionChanged: func(condition networkmonitor.NetworkConditionType) {
			client.onNetworkConditionChanged(condition)
		},
		onEndedCallbacks: make([]func(), 0),
	}

	t.context, t.cancel = context.WithCancel(client.Context())

	rt := t.AddRemoteTrack(track, minWait, maxWait, stats, onStatsUpdated, onPLI)

	rt.OnEnded(func() {
		t.cancel()
		t.onEnded()
	})

	return t
}

func (t *SimulcastTrack) ClientID() string {
	return t.base.client.id
}

func (t *SimulcastTrack) Context() context.Context {
	return t.context
}

func (t *SimulcastTrack) onRemoteTrackAdded(f func(*remoteTrack)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onAddedRemoteTrackCallbacks = append(t.onAddedRemoteTrackCallbacks, f)
}

func (t *SimulcastTrack) onRemoteTrackAddedCallbacks(track *remoteTrack) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, f := range t.onAddedRemoteTrackCallbacks {
		f(track)
	}
}

func (t *SimulcastTrack) OnTrackComplete(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackCompleteCallbacks = append(t.onTrackCompleteCallbacks, f)
}

func (t *SimulcastTrack) onTrackComplete() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, f := range t.onTrackCompleteCallbacks {
		f()
	}
}

// TODO: this is contain multiple tracks, there is a possibility remote track high is not available yet
func (t *SimulcastTrack) ID() string {
	return t.base.id
}

func (t *SimulcastTrack) StreamID() string {
	return t.base.streamid
}

func (t *SimulcastTrack) IsSimulcast() bool {
	return true
}

func (t *SimulcastTrack) IsScaleable() bool {
	return false
}

func (t *SimulcastTrack) IsProcessed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.isProcessed
}

func (t *SimulcastTrack) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *SimulcastTrack) AddRemoteTrack(track IRemoteTrack, minWait, maxWait time.Duration, stats stats.Getter, onStatsUpdated func(*stats.Stats), onPLI func()) *remoteTrack {
	var remoteTrack *remoteTrack

	quality := RIDToQuality(track.RID())

	onRead := func(attrs interceptor.Attributes, p *rtp.Packet) {

		// set the base timestamp for the track if it is not set yet
		if t.baseTS == 0 {
			t.baseTS = p.Timestamp
		}

		if quality == QualityHigh && t.remoteTrackHighBaseTS == 0 {
			t.remoteTrackHighBaseTS = p.Timestamp
		} else if quality == QualityMid && t.remoteTrackMidBaseTS == 0 {
			t.remoteTrackMidBaseTS = p.Timestamp
		} else if quality == QualityLow && t.remoteTrackLowBaseTS == 0 {
			t.remoteTrackLowBaseTS = p.Timestamp
		}

		readTime := time.Now().UnixNano()

		switch quality {
		case QualityHigh:
			t.lastReadHighTS.Store(readTime)
			t.lastHighSequence = t.highSequence
			t.highSequence = p.SequenceNumber
		case QualityMid:
			t.lastReadMidTS.Store(readTime)
			t.lastMidSequence = t.midSequence
			t.midSequence = p.SequenceNumber
		case QualityLow:
			t.lastReadLowTS.Store(readTime)
			t.lastLowSequence = t.lowSequence
			t.lowSequence = p.SequenceNumber
		}

		tracks := t.base.clientTracks.GetTracks()
		for _, track := range tracks {
			copyPacket := t.base.pool.CopyPacket(p)

			track.push(copyPacket, quality)

			t.base.pool.PutPacket(copyPacket)

		}

		copyPacket := t.base.pool.CopyPacket(p)

		t.onRead(attrs, copyPacket, quality)

		t.base.pool.PutPacket(copyPacket)

	}

	remoteTrack = newRemoteTrack(t.Context(), t.base.client.log, t.reordered, track, minWait, maxWait, t.pliInterval, onPLI, stats, onStatsUpdated, onRead, t.base.pool, t.onNetworkConditionChanged)

	switch quality {
	case QualityHigh:
		t.mu.Lock()
		t.remoteTrackHigh = remoteTrack
		t.mu.Unlock()

		remoteTrack.OnEnded(func() {
			t.mu.Lock()
			t.remoteTrackHigh = nil
			t.mu.Unlock()
			t.cancel()
			t.onEnded()
		})

	case QualityMid:
		t.mu.Lock()
		t.remoteTrackMid = remoteTrack
		t.mu.Unlock()

		remoteTrack.OnEnded(func() {
			t.mu.Lock()
			t.remoteTrackMid = nil
			t.mu.Unlock()
			t.cancel()
			t.onEnded()
		})

	case QualityLow:
		t.mu.Lock()
		t.remoteTrackLow = remoteTrack
		t.mu.Unlock()

		remoteTrack.OnEnded(func() {
			t.mu.Lock()
			t.remoteTrackLow = nil
			t.mu.Unlock()
			t.cancel()
			t.onEnded()
		})
	default:
		t.base.client.log.Warnf("client: unknown track quality ", track.RID())
		return nil
	}

	// check if all simulcast tracks are available
	if t.remoteTrackHigh != nil && t.remoteTrackMid != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}

	t.onRemoteTrackAddedCallbacks(remoteTrack)

	return remoteTrack
}

func (t *SimulcastTrack) GetRemoteTrack(q QualityLevel) *remoteTrack {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch q {
	case QualityHigh:
		return t.remoteTrackHigh
	case QualityMid:
		return t.remoteTrackMid
	case QualityLow:
		return t.remoteTrackLow
	}

	return nil
}

func (t *SimulcastTrack) subscribe(client *Client) iClientTrack {
	// Create a local track, all our SFU clients will be fed via this track

	ct := newSimulcastClientTrack(client, t)

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *SimulcastTrack) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *SimulcastTrack) SourceType() TrackType {
	if t.base.isScreen.Load() {
		return TrackTypeScreen
	}

	return TrackTypeMedia
}

func (t *SimulcastTrack) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *SimulcastTrack) IsScreen() bool {
	return t.base.isScreen.Load()
}

func (t *SimulcastTrack) IsTrackComplete() bool {
	return t.TotalTracks() == 3
}

func (t *SimulcastTrack) TotalTracks() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	total := 0
	if t.remoteTrackHigh != nil {
		total++
	}

	if t.remoteTrackMid != nil {
		total++
	}

	if t.remoteTrackLow != nil {
		total++
	}

	return total
}

// track is considered active if the track is not nil and the latest read operation was 500ms ago
func (t *SimulcastTrack) isTrackActive(quality QualityLevel) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// set max active track threshold to 500ms
	threshold := time.Duration(500) * time.Millisecond

	switch quality {
	case QualityHigh:
		if t.remoteTrackHigh == nil {
			t.base.client.log.Warnf("track: remote track high is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadHighTS.Load()))

		if delta > threshold {
			t.base.client.log.Warnf("track: remote track %s high is not active, last read was %d ms ago", t.base.id, delta.Milliseconds())
			return false
		}

		return true
	case QualityMid:
		if t.remoteTrackMid == nil {
			t.base.client.log.Warnf("track: remote track medium is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadMidTS.Load()))
		if delta > threshold {
			t.base.client.log.Warnf("track: remote track %s mid is not active, last read was %d ms ago", delta.Milliseconds())
			return false
		}

		return true
	case QualityLow:
		if t.remoteTrackLow == nil {
			t.base.client.log.Warnf("track: remote track low is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadLowTS.Load()))
		if delta > threshold {
			t.base.client.log.Warnf("track: remote track %s low is not active, last read was %d ms ago", delta.Milliseconds())
			return false
		}

		return true
	}

	return false
}

func (t *SimulcastTrack) sendPLI() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackHigh != nil {
		t.remoteTrackHigh.SendPLI()
	} else {
		t.base.client.log.Warnf("track: remote track high is nil")
	}

	if t.remoteTrackMid != nil {
		t.remoteTrackMid.SendPLI()
	} else {
		t.base.client.log.Warnf("track: remote track mid is nil")
	}

	if t.remoteTrackLow != nil {
		t.remoteTrackLow.SendPLI()
	} else {
		t.base.client.log.Warnf("track: remote track low is nil")
	}
}

func (t *SimulcastTrack) MimeType() string {
	return t.base.codec.MimeType
}

func (t *SimulcastTrack) OnRead(callback func(interceptor.Attributes, *rtp.Packet, QualityLevel)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onReadCallbacks = append(t.onReadCallbacks, callback)
}

func (t *SimulcastTrack) onRead(attr interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
	for _, callback := range t.onReadCallbacks {
		callback(attr, p, quality)
	}
}

func (t *SimulcastTrack) SSRCHigh() webrtc.SSRC {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackHigh == nil {
		return 0
	}

	return t.remoteTrackHigh.Track().SSRC()
}

func (t *SimulcastTrack) SSRCMid() webrtc.SSRC {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackMid == nil {
		return 0
	}

	return t.remoteTrackMid.Track().SSRC()
}

func (t *SimulcastTrack) SSRCLow() webrtc.SSRC {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackLow == nil {
		return 0
	}

	return t.remoteTrackLow.Track().SSRC()
}

func (t *SimulcastTrack) RIDHigh() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackHigh == nil {
		return ""
	}

	return t.remoteTrackHigh.track.RID()
}

func (t *SimulcastTrack) RIDMid() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackMid == nil {
		return ""
	}

	return t.remoteTrackMid.track.RID()
}

func (t *SimulcastTrack) RIDLow() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackLow == nil {
		return ""
	}

	return t.remoteTrackLow.track.RID()
}

func (t *SimulcastTrack) Relay(f func(webrtc.SSRC, interceptor.Attributes, *rtp.Packet)) {
	t.OnRead(func(attrs interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
		switch quality {
		case QualityHigh:
			f(t.SSRCHigh(), attrs, p)
		case QualityMid:
			f(t.SSRCMid(), attrs, p)
		case QualityLow:
			f(t.SSRCLow(), attrs, p)
		}
	})
}

func (t *SimulcastTrack) PayloadType() webrtc.PayloadType {
	return t.base.codec.PayloadType
}

func (t *SimulcastTrack) IsRelay() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.remoteTrackHigh != nil {
		return t.remoteTrackHigh.IsRelay()
	} else if t.remoteTrackMid != nil {
		return t.remoteTrackMid.IsRelay()
	} else if t.remoteTrackLow != nil {
		return t.remoteTrackLow.IsRelay()
	}

	return false
}

func (t *SimulcastTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *SimulcastTrack) onEnded() {
	for _, f := range t.onEndedCallbacks {
		f()
	}
}

type SubscribeTrackRequest struct {
	ClientID string `json:"client_id"`
	TrackID  string `json:"track_id"`
}

type trackList struct {
	tracks map[string]ITrack
	mu     sync.RWMutex
	log    logging.LeveledLogger
}

func newTrackList(log logging.LeveledLogger) *trackList {
	return &trackList{
		tracks: make(map[string]ITrack),
		log:    log,
	}
}

func (t *trackList) Add(track ITrack) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := track.ID()
	if _, ok := t.tracks[id]; ok {
		t.log.Warnf("tracklist: track  %s already added", id)
		return ErrTrackExists
	}

	t.tracks[id] = track

	return nil
}

func (t *trackList) Get(ID string) (ITrack, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if track, ok := t.tracks[ID]; ok {
		return track, nil
	}

	return nil, ErrTrackIsNotExists
}

//nolint:copylocks // This is a read only operation
func (t *trackList) remove(ids []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, id := range ids {
		delete(t.tracks, id)
	}

}

func (t *trackList) Reset() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	t.tracks = make(map[string]ITrack)
}

func (t *trackList) GetTracks() []ITrack {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tracks := make([]ITrack, 0)
	for _, track := range t.tracks {
		tracks = append(tracks, track)
	}

	return tracks
}

func (t *trackList) Length() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return len(t.tracks)
}

func RIDToQuality(RID string) QualityLevel {
	switch RID {
	case "high":
		return QualityHigh
	case "mid":
		return QualityMid
	default:
		return QualityLow
	}
}
