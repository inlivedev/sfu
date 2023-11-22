package sfu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
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
	clientid     string
	isProcessed  bool
	kind         webrtc.RTPCodecType
	codec        webrtc.RTPCodecParameters
	isScreen     *atomic.Bool // source of the track, can be media or screen
	clientTracks *clientTrackList
}

type ITrack interface {
	ID() string
	StreamID() string
	ClientID() string
	IsSimulcast() bool
	IsScaleable() bool
	IsProcessed() bool
	SetSourceType(TrackType)
	SetAsProcessed()
	OnRead(func(rtp.Packet, QualityLevel))
	IsScreen() bool
	Kind() webrtc.RTPCodecType
	MimeType() string
	TotalTracks() int
	Context() context.Context
	Relay(func(webrtc.SSRC, rtp.Packet))
}

type Track struct {
	context          context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	base             baseTrack
	remoteTrack      *remoteTrack
	onEndedCallbacks []func()
	onReadCallbacks  []func(rtp.Packet, QualityLevel)
}

func newTrack(ctx context.Context, clientID string, trackRemote IRemoteTrack, pliInterval time.Duration, onPLI func() error, stats stats.Getter, onStatsUpdated func(*stats.Stats)) ITrack {
	ctList := newClientTrackList()

	localCtx, cancel := context.WithCancel(ctx)

	baseTrack := baseTrack{
		id:           trackRemote.ID(),
		isScreen:     &atomic.Bool{},
		msid:         trackRemote.Msid(),
		streamid:     trackRemote.StreamID(),
		clientid:     clientID,
		kind:         trackRemote.Kind(),
		codec:        trackRemote.Codec(),
		clientTracks: ctList,
	}

	t := &Track{
		mu:               sync.Mutex{},
		context:          localCtx,
		cancel:           cancel,
		base:             baseTrack,
		onReadCallbacks:  make([]func(rtp.Packet, QualityLevel), 0),
		onEndedCallbacks: make([]func(), 0),
	}

	onRead := func(p rtp.Packet) {
		// do
		tracks := t.base.clientTracks.GetTracks()
		for _, track := range tracks {
			track.push(p, QualityHigh) // quality doesn't matter on non simulcast track
		}

		t.onRead(p, QualityHigh)
	}

	t.remoteTrack = newRemoteTrack(localCtx, trackRemote, pliInterval, onPLI, stats, onStatsUpdated, onRead)

	return t
}

func (t *Track) ClientID() string {
	return t.base.clientid
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

func (t *Track) ID() string {
	return t.base.id
}

func (t *Track) StreamID() string {
	return t.base.streamid
}

func (t *Track) SSRC() webrtc.SSRC {
	return t.remoteTrack.track.SSRC()
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

	ctx, cancel := context.WithCancel(t.Context())

	if t.MimeType() == webrtc.MimeTypeVP9 {
		ct = &scaleableClientTrack{
			context:               ctx,
			cancel:                cancel,
			mu:                    sync.RWMutex{},
			id:                    t.base.id,
			kind:                  t.base.kind,
			mimeType:              t.base.codec.MimeType,
			client:                c,
			localTrack:            t.createLocalTrack(),
			remoteTrack:           t,
			isScreen:              false,
			onTrackEndedCallbacks: make([]func(), 0),
			qualityPreset:         c.SFU().QualityPreset(),
			maxQuality:            QualityHigh,
			lastQuality:           QualityHigh,
		}
	} else {
		isScreen := &atomic.Bool{}
		isScreen.Store(t.IsScreen())

		ct = &clientTrack{
			id:          t.base.id,
			context:     ctx,
			cancel:      cancel,
			mu:          sync.RWMutex{},
			client:      c,
			kind:        t.base.kind,
			mimeType:    t.remoteTrack.track.Codec().MimeType,
			localTrack:  t.createLocalTrack(),
			remoteTrack: t.remoteTrack,
			isScreen:    isScreen,
		}
	}

	if t.Kind() == webrtc.RTPCodecTypeAudio && c.IsVADEnabled() {
		glog.Info("track: voice activity detector enabled")
		vad := c.vad.AddAudioTrack(ct.LocalTrack())
		vad.OnVoiceDetected(func(activity voiceactivedetector.VoiceActivity) {
			// send through datachannel
			c.onVoiceDetected(activity)
		})
	}

	go func() {
		defer cancel()
		<-ctx.Done()
	}()

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *Track) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *Track) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *Track) OnRead(callback func(rtp.Packet, QualityLevel)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onReadCallbacks = append(t.onReadCallbacks, callback)
}

func (t *Track) onRead(p rtp.Packet, quality QualityLevel) {
	// t.mu.Lock()
	// defer t.mu.Unlock()

	for _, callback := range t.onReadCallbacks {
		callback(p, quality)
	}
}

func (t *Track) Relay(f func(webrtc.SSRC, rtp.Packet)) {
	t.OnRead(func(p rtp.Packet, quality QualityLevel) {
		f(t.SSRC(), p)
	})
}

type SimulcastTrack struct {
	context                     context.Context
	mu                          sync.Mutex
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
	onReadCallbacks             []func(rtp.Packet, QualityLevel)
	pliInterval                 time.Duration
	onPLI                       func() error
}

func newSimulcastTrack(ctx context.Context, clientid string, track IRemoteTrack, pliInterval time.Duration, onPLI func() error, stats stats.Getter, onStatsUpdated func(*stats.Stats)) ITrack {
	t := &SimulcastTrack{
		context: ctx,
		mu:      sync.Mutex{},
		base: &baseTrack{
			id:           track.ID(),
			isScreen:     &atomic.Bool{},
			msid:         track.Msid(),
			streamid:     track.StreamID(),
			clientid:     clientid,
			kind:         track.Kind(),
			codec:        track.Codec(),
			clientTracks: newClientTrackList(),
		},
		lastReadHighTS:              &atomic.Int64{},
		lastReadMidTS:               &atomic.Int64{},
		lastReadLowTS:               &atomic.Int64{},
		lastHighKeyframeTS:          &atomic.Int64{},
		lastMidKeyframeTS:           &atomic.Int64{},
		lastLowKeyframeTS:           &atomic.Int64{},
		onTrackCompleteCallbacks:    make([]func(), 0),
		onAddedRemoteTrackCallbacks: make([]func(*remoteTrack), 0),
		onReadCallbacks:             make([]func(rtp.Packet, QualityLevel), 0),
		pliInterval:                 pliInterval,
		onPLI:                       onPLI,
	}

	_ = t.AddRemoteTrack(track, stats, onStatsUpdated)

	return t
}

func (t *SimulcastTrack) ClientID() string {
	return t.base.clientid
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

func (t *SimulcastTrack) AddRemoteTrack(track IRemoteTrack, stats stats.Getter, onStatsUpdated func(*stats.Stats)) *remoteTrack {
	var remoteTrack *remoteTrack

	quality := RIDToQuality(track.RID())

	onRead := func(p rtp.Packet) {
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
			track.push(p, quality)
		}

		t.onRead(p, quality)
	}

	remoteTrack = newRemoteTrack(t.context, track, t.pliInterval, t.onPLI, stats, onStatsUpdated, onRead)

	switch quality {
	case QualityHigh:
		t.mu.Lock()
		t.remoteTrackHigh = remoteTrack
		t.mu.Unlock()

		go func() {
			ctx, cancel := context.WithCancel(remoteTrack.Context())
			defer cancel()
			<-ctx.Done()
			t.mu.Lock()
			t.remoteTrackHigh = nil
			t.mu.Unlock()
		}()

	case QualityMid:
		t.mu.Lock()
		t.remoteTrackMid = remoteTrack
		t.mu.Unlock()

		go func() {
			ctx, cancel := context.WithCancel(remoteTrack.Context())
			defer cancel()
			<-ctx.Done()
			t.mu.Lock()
			t.remoteTrackMid = nil
			t.mu.Unlock()
		}()

	case QualityLow:
		t.mu.Lock()
		t.remoteTrackLow = remoteTrack
		t.mu.Unlock()

		go func() {
			ctx, cancel := context.WithCancel(remoteTrack.Context())
			defer cancel()
			<-ctx.Done()
			t.mu.Lock()
			t.remoteTrackLow = nil
			t.mu.Unlock()
		}()
	default:
		glog.Warning("client: unknown track quality ", track.RID())
		return nil
	}

	// check if all simulcast tracks are available
	if t.remoteTrackHigh != nil && t.remoteTrackMid != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}

	t.onRemoteTrackAddedCallbacks(remoteTrack)

	return remoteTrack
}

func (t *SimulcastTrack) getRemoteTrack(q QualityLevel) *remoteTrack {
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
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.base.codec.RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	isScreen := &atomic.Bool{}
	isScreen.Store(t.IsScreen())

	lastQuality := &atomic.Uint32{}

	sequenceNumber := &atomic.Uint32{}

	lastTimestamp := &atomic.Uint32{}

	ctx, cancel := context.WithCancel(t.Context())

	ct := &simulcastClientTrack{
		mu:                      sync.RWMutex{},
		id:                      t.base.id,
		context:                 ctx,
		cancel:                  cancel,
		kind:                    t.base.kind,
		mimeType:                t.base.codec.MimeType,
		client:                  client,
		localTrack:              track,
		remoteTrack:             t,
		sequenceNumber:          sequenceNumber,
		lastQuality:             lastQuality,
		paddingQuality:          &atomic.Uint32{},
		paddingTS:               &atomic.Uint32{},
		maxQuality:              &atomic.Uint32{},
		lastBlankSequenceNumber: &atomic.Uint32{},
		lastTimestamp:           lastTimestamp,
		isScreen:                isScreen,
		isEnded:                 &atomic.Bool{},
		onTrackEndedCallbacks:   make([]func(), 0),
	}

	ct.SetMaxQuality(QualityHigh)

	go func() {
		defer cancel()
		<-ctx.Done()
	}()

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *SimulcastTrack) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
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
	t.mu.Lock()
	defer t.mu.Unlock()

	// set max active track threshold to 500ms
	threshold := time.Duration(500) * time.Millisecond

	switch quality {
	case QualityHigh:
		if t.remoteTrackHigh == nil {
			glog.Warning("track: remote track high is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadHighTS.Load()))

		if delta > threshold {
			glog.Warningf("track: remote track %s high is not active, last read was %d ms ago", delta.Milliseconds())
			return false
		}

		return true
	case QualityMid:
		if t.remoteTrackMid == nil {
			glog.Warning("track: remote track medium is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadMidTS.Load()))
		if delta > threshold {
			glog.Warningf("track: remote track %s mid is not active, last read was %d ms ago", delta.Milliseconds())
			return false
		}

		return true
	case QualityLow:
		if t.remoteTrackLow == nil {
			glog.Warning("track: remote track low is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadLowTS.Load()))
		if delta > threshold {
			glog.Warningf("track: remote track %s low is not active, last read was %d ms ago", delta.Milliseconds())
			return false
		}

		return true
	}

	return false
}

func (t *SimulcastTrack) sendPLI(quality QualityLevel) {
	switch quality {
	case QualityHigh:
		if t.remoteTrackHigh != nil {
			if err := t.remoteTrackHigh.sendPLI(); err != nil {
				glog.Error("client: error sending PLI ", err)
			}
		}
	case QualityMid:
		if t.remoteTrackMid != nil {
			if err := t.remoteTrackMid.sendPLI(); err != nil {
				glog.Error("client: error sending PLI ", err)
			}
		}
	case QualityLow:
		if t.remoteTrackLow != nil {
			if err := t.remoteTrackLow.sendPLI(); err != nil {
				glog.Error("client: error sending PLI ", err)
			}
		}
	}
}

func (t *SimulcastTrack) MimeType() string {
	return t.base.codec.MimeType
}

func (t *SimulcastTrack) OnRead(callback func(rtp.Packet, QualityLevel)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onReadCallbacks = append(t.onReadCallbacks, callback)
}

func (t *SimulcastTrack) onRead(p rtp.Packet, quality QualityLevel) {
	for _, callback := range t.onReadCallbacks {
		callback(p, quality)
	}
}

func (t *SimulcastTrack) SSRCHigh() webrtc.SSRC {
	if t.remoteTrackHigh == nil {
		return 0
	}

	return t.remoteTrackHigh.Track().SSRC()
}

func (t *SimulcastTrack) SSRCMid() webrtc.SSRC {
	if t.remoteTrackMid == nil {
		return 0
	}

	return t.remoteTrackMid.Track().SSRC()
}

func (t *SimulcastTrack) SSRCLow() webrtc.SSRC {
	if t.remoteTrackLow == nil {
		return 0
	}

	return t.remoteTrackLow.Track().SSRC()
}

func (t *SimulcastTrack) RIDHigh() string {
	if t.remoteTrackHigh == nil {
		return ""
	}

	return t.remoteTrackHigh.track.RID()
}

func (t *SimulcastTrack) RIDMid() string {
	if t.remoteTrackMid == nil {
		return ""
	}

	return t.remoteTrackMid.track.RID()
}

func (t *SimulcastTrack) RIDLow() string {
	if t.remoteTrackLow == nil {
		return ""
	}

	return t.remoteTrackLow.track.RID()
}

func (t *SimulcastTrack) Relay(f func(webrtc.SSRC, rtp.Packet)) {
	t.OnRead(func(p rtp.Packet, quality QualityLevel) {
		switch quality {
		case QualityHigh:
			f(t.SSRCHigh(), p)
		case QualityMid:
			f(t.SSRCMid(), p)
		case QualityLow:
			f(t.SSRCLow(), p)
		}
	})
}

type SubscribeTrackRequest struct {
	ClientID string `json:"client_id"`
	StreamID string `json:"stream_id"`
	TrackID  string `json:"track_id"`
	RID      string `json:"rid"`
}

type trackList struct {
	tracks map[string]ITrack
	mu     sync.RWMutex
}

func newTrackList() *trackList {
	return &trackList{
		tracks: make(map[string]ITrack),
	}
}

func (t *trackList) Add(track ITrack) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := track.ID()
	if _, ok := t.tracks[id]; ok {
		glog.Warning("client: track already added ", id)
		return ErrTrackExists
	}

	t.tracks[id] = track

	go func() {
		ctx, cancel := context.WithCancel(track.Context())
		defer cancel()
		<-ctx.Done()
		t.remove([]string{id})
	}()

	return nil
}

func (t *trackList) Get(ID string) (ITrack, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

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
	t.mu.Lock()
	defer t.mu.Unlock()

	tracks := make([]ITrack, 0)
	for _, track := range t.tracks {
		tracks = append(tracks, track)
	}

	return tracks
}

func (t *trackList) Length() int {
	t.mu.Lock()
	defer t.mu.Unlock()

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
