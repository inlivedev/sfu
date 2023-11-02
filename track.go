package sfu

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
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
	client       *Client
	isProcessed  bool
	kind         webrtc.RTPCodecType
	codec        webrtc.RTPCodecParameters
	isScreen     *atomic.Bool // source of the track, can be media or screen
	clientTracks *clientTrackList
}

type ITrack interface {
	ID() string
	StreamID() string
	Client() *Client
	IsSimulcast() bool
	IsScaleable() bool
	IsProcessed() bool
	SetSourceType(TrackType)
	SetAsProcessed()
	IsScreen() bool
	Kind() webrtc.RTPCodecType
	MimeType() string
	TotalTracks() int
	OnEnded(func())
	onEnded()
}

type track struct {
	mu               sync.Mutex
	base             baseTrack
	remoteTrack      *remoteTrack
	onEndedCallbacks []func()
	vad              *voiceactivedetector.VoiceDetector
}

func newTrack(client *Client, trackRemote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, vad *voiceactivedetector.VoiceDetector) ITrack {
	ctList := newClientTrackList()
	onTrackRead := func(p *rtp.Packet) {
		// do
		tracks := ctList.GetTracks()
		for _, track := range tracks {
			track.push(p, QualityHigh) // quality doesn't matter on non simulcast track
		}
	}

	baseTrack := baseTrack{
		id:           trackRemote.ID(),
		isScreen:     &atomic.Bool{},
		msid:         trackRemote.Msid(),
		streamid:     trackRemote.StreamID(),
		client:       client,
		kind:         trackRemote.Kind(),
		codec:        trackRemote.Codec(),
		clientTracks: ctList,
	}

	t := &track{
		mu:          sync.Mutex{},
		base:        baseTrack,
		remoteTrack: newRemoteTrack(client, trackRemote, receiver, onTrackRead),
		vad:         vad,
	}

	t.remoteTrack.OnEnded(func() {
		t.onEnded()
	})

	return t
}

func (t *track) createLocalTrack() *webrtc.TrackLocalStaticRTP {
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrack.track.Codec().RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	return track
}

func (t *track) ID() string {
	return t.base.id
}

func (t *track) StreamID() string {
	return t.base.streamid
}

func (t *track) Client() *Client {
	return t.base.client
}

func (t *track) RemoteTrack() *remoteTrack {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.remoteTrack
}

func (t *track) IsScreen() bool {
	return t.base.isScreen.Load()
}

func (t *track) IsSimulcast() bool {
	return false
}

func (t *track) IsScaleable() bool {
	return t.MimeType() == webrtc.MimeTypeVP9
}

func (t *track) IsProcessed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.isProcessed
}

func (t *track) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *track) MimeType() string {
	return t.base.codec.MimeType
}

func (t *track) TotalTracks() int {
	return 1
}

func (t *track) subscribe(c *Client) iClientTrack {
	var ct iClientTrack

	if t.Kind() == webrtc.RTPCodecTypeAudio && c.IsVADEnabled() {
		glog.Info("track: voice activity detector enabled")
		t.vad.OnVoiceDetected(func(activity voiceactivedetector.VoiceActivity) {
			// send through datachannel
			c.onVoiceDetected(activity)
		})
	}

	if t.MimeType() == webrtc.MimeTypeVP9 {
		ct = &scaleabletClientTrack{
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
			id:                    t.base.id,
			mu:                    sync.RWMutex{},
			client:                c,
			kind:                  t.base.kind,
			mimeType:              t.remoteTrack.track.Codec().MimeType,
			localTrack:            t.createLocalTrack(),
			remoteTrack:           t.remoteTrack,
			isScreen:              isScreen,
			onTrackEndedCallbacks: make([]func(), 0),
		}
	}

	t.remoteTrack.OnEnded(func() {
		ct.onTrackEnded()
	})

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *track) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *track) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *track) onEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.onEndedCallbacks {
		callback()
	}
}

func (t *track) OnEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, callback)
}

type simulcastTrack struct {
	mu                          sync.Mutex
	base                        *baseTrack
	baseTS                      uint32
	onTrackComplete             func()
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
	onEndedCallbacks            []func()
}

func newSimulcastTrack(client *Client, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) ITrack {
	t := &simulcastTrack{
		mu: sync.Mutex{},
		base: &baseTrack{
			id:           track.ID(),
			isScreen:     &atomic.Bool{},
			msid:         track.Msid(),
			streamid:     track.StreamID(),
			client:       client,
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
		onAddedRemoteTrackCallbacks: make([]func(*remoteTrack), 0),
		onEndedCallbacks:            make([]func(), 0),
	}

	_ = t.AddRemoteTrack(track, receiver)

	return t
}

func (t *simulcastTrack) onRemoteTrackAdded(f func(*remoteTrack)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onAddedRemoteTrackCallbacks = append(t.onAddedRemoteTrackCallbacks, f)
}

func (t *simulcastTrack) onRemoteTrackAddedCallbacks(track *remoteTrack) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, f := range t.onAddedRemoteTrackCallbacks {
		f(track)
	}
}

func (t *simulcastTrack) OnTrackComplete(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackComplete = f
}

// TODO: this is contain multiple tracks, there is a possibility remote track high is not available yet
func (t *simulcastTrack) ID() string {
	return t.base.id
}

func (t *simulcastTrack) StreamID() string {
	return t.base.streamid
}

func (t *simulcastTrack) Client() *Client {
	return t.base.client
}

func (t *simulcastTrack) IsSimulcast() bool {
	return true
}

func (t *simulcastTrack) IsScaleable() bool {
	return false
}

func (t *simulcastTrack) IsProcessed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.isProcessed
}

func (t *simulcastTrack) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *simulcastTrack) AddRemoteTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) *remoteTrack {
	var remoteTrack *remoteTrack

	quality := RIDToQuality(track.RID())

	onRead := func(p *rtp.Packet) {
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
	}

	t.mu.Lock()

	switch quality {
	case QualityHigh:
		remoteTrack = newRemoteTrack(t.base.client, track, receiver, onRead)
		t.remoteTrackHigh = remoteTrack
		remoteTrack.OnEnded(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.remoteTrackHigh = nil

			if t.remoteTrackHigh == nil && t.remoteTrackMid == nil && t.remoteTrackLow == nil {
				t.onEnded()
			}
		})

	case QualityMid:
		remoteTrack = newRemoteTrack(t.base.client, track, receiver, onRead)
		t.remoteTrackMid = remoteTrack
		remoteTrack.OnEnded(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.remoteTrackMid = nil

			if t.remoteTrackHigh == nil && t.remoteTrackMid == nil && t.remoteTrackLow == nil {
				t.onEnded()
			}
		})
	case QualityLow:
		remoteTrack = newRemoteTrack(t.base.client, track, receiver, onRead)
		t.remoteTrackLow = remoteTrack
		remoteTrack.OnEnded(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.remoteTrackLow = nil

			if t.remoteTrackHigh == nil && t.remoteTrackMid == nil && t.remoteTrackLow == nil {
				t.onEnded()
			}
		})
	default:
		glog.Warning("client: unknown track quality ", track.RID())
		return nil
	}

	// check if all simulcast tracks are available
	if t.onTrackComplete != nil && t.remoteTrackHigh != nil && t.remoteTrackMid != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}

	t.mu.Unlock()

	t.onRemoteTrackAddedCallbacks(remoteTrack)

	return remoteTrack
}

func (t *simulcastTrack) getRemoteTrack(q QualityLevel) *remoteTrack {
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

func (t *simulcastTrack) subscribe(client *Client) iClientTrack {
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

	ct := &simulcastClientTrack{
		mu:                      sync.RWMutex{},
		id:                      t.base.id,
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

	if t.remoteTrackLow != nil {
		t.remoteTrackLow.OnEnded(func() {
			ct.onTrackEnded()
		})
	}

	if t.remoteTrackMid != nil {
		t.remoteTrackMid.OnEnded(func() {
			ct.onTrackEnded()
		})
	}
	if t.remoteTrackHigh != nil {
		t.remoteTrackHigh.OnEnded(func() {
			ct.onTrackEnded()
		})
	}

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *simulcastTrack) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *simulcastTrack) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *simulcastTrack) IsScreen() bool {
	return t.base.isScreen.Load()
}

func (t *simulcastTrack) TotalTracks() int {
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
func (t *simulcastTrack) isTrackActive(quality QualityLevel) bool {
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
			glog.Warningf("track: remote track %s high is not active, last read was %d ms ago", t.Client().ID(), delta.Milliseconds())
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
			glog.Warningf("track: remote track %s mid is not active, last read was %d ms ago", t.Client().ID(), delta.Milliseconds())
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
			glog.Warningf("track: remote track %s low is not active, last read was %d ms ago", t.Client().ID(), delta.Milliseconds())
			return false
		}

		return true
	}

	return false
}

func (t *simulcastTrack) sendPLI(quality QualityLevel) {
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

func (t *simulcastTrack) MimeType() string {
	return t.base.codec.MimeType
}

func (t *simulcastTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *simulcastTrack) onEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.onEndedCallbacks {
		callback()
	}
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

	track.OnEnded(func() {
		t.remove([]string{id})
	})

	return nil
}

func (t *trackList) Get(ID string) (ITrack, error) {
	// t.mu.Lock()
	// defer t.mu.Unlock()

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
