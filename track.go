package sfu

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtcp"
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

type BaseTrack struct {
	id           string
	msid         string
	streamid     string
	client       *Client
	isProcessed  bool
	kind         webrtc.RTPCodecType
	codec        webrtc.RTPCodecParameters
	sourceType   TrackType // source of the track, can be media or screen
	clientTracks *clientTrackList
}

type ITrack interface {
	ID() string
	Client() *Client
	IsSimulcast() bool
	IsProcessed() bool
	SetSourceType(TrackType)
	SetAsProcessed()
	SourceType() TrackType
	Kind() webrtc.RTPCodecType
	TotalTracks() int
}

type Track struct {
	mu          sync.Mutex
	base        BaseTrack
	remoteTrack *RemoteTrack
}

func NewTrack(client *Client, track *webrtc.TrackRemote) ITrack {
	ctList := newClientTrackList()
	onTrackRead := func(p *rtp.Packet) {
		// do
		tracks := ctList.GetTracks()
		for _, track := range tracks {
			track.push(p, QualityHigh) // quality doesn't matter on non simulcast track
		}
	}

	baseTrack := BaseTrack{
		id:           track.ID(),
		msid:         track.Msid(),
		streamid:     track.StreamID(),
		client:       client,
		kind:         track.Kind(),
		codec:        track.Codec(),
		clientTracks: ctList,
	}

	t := &Track{
		mu:          sync.Mutex{},
		base:        baseTrack,
		remoteTrack: NewRemoteTrack(client, track, onTrackRead),
	}

	return t
}

func (t *Track) createLocalTrack() *webrtc.TrackLocalStaticRTP {
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrack.track.Codec().RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	return track
}

func (t *Track) ID() string {
	return t.base.msid
}

func (t *Track) Client() *Client {
	return t.base.client
}

func (t *Track) RemoteTrack() *RemoteTrack {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.remoteTrack
}

func (t *Track) SourceType() TrackType {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.sourceType
}

func (t *Track) IsSimulcast() bool {
	return false
}

func (t *Track) IsProcessed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.isProcessed
}

func (t *Track) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *Track) TotalTracks() int {
	return 1
}

func (t *Track) Subscribe() iClientTrack {
	ct := &ClientTrack{
		id:          t.base.id,
		client:      t.Client(),
		kind:        t.base.kind,
		mimeType:    t.remoteTrack.track.Codec().MimeType,
		localTrack:  t.createLocalTrack(),
		remoteTrack: t.remoteTrack,
		sourceType:  t.base.sourceType,
	}

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *Track) SetSourceType(sourceType TrackType) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.sourceType = sourceType
}

func (t *Track) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *Track) SendPLI() error {
	return t.base.client.peerConnection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(t.remoteTrack.track.SSRC())},
	})
}

func (t *Track) GetSourceType() TrackType {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.sourceType
}

type SimulcastTrack struct {
	mu                    sync.Mutex
	base                  *BaseTrack
	baseTS                uint32
	onTrackComplete       func()
	remoteTrackHigh       *RemoteTrack
	remoteTrackHighBaseTS uint32
	remoteTrackMid        *RemoteTrack
	remoteTrackMidBaseTS  uint32
	remoteTrackLow        *RemoteTrack
	remoteTrackLowBaseTS  uint32
	lastReadHighTimestamp atomic.Int64
	lastReadMidTimestamp  atomic.Int64
	lastReadLowTimestamp  atomic.Int64
}

func NewSimulcastTrack(client *Client, track *webrtc.TrackRemote) ITrack {
	t := &SimulcastTrack{
		mu: sync.Mutex{},
		base: &BaseTrack{
			id:           track.ID(),
			msid:         track.Msid(),
			streamid:     track.StreamID(),
			client:       client,
			kind:         track.Kind(),
			codec:        track.Codec(),
			clientTracks: newClientTrackList(),
		},
	}

	t.AddRemoteTrack(track)

	return t
}

func (t *SimulcastTrack) OnTrackComplete(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackComplete = f
}

// TODO: this is contain multiple tracks, there is a possibility remote track high is not available yet
func (t *SimulcastTrack) ID() string {
	return t.base.msid
}

func (t *SimulcastTrack) Client() *Client {
	return t.base.client
}

func (t *SimulcastTrack) IsSimulcast() bool {
	return true
}

func (t *SimulcastTrack) IsProcessed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.isProcessed
}

func (t *SimulcastTrack) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *SimulcastTrack) AddRemoteTrack(track *webrtc.TrackRemote) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var remoteTrack *RemoteTrack

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
			t.lastReadHighTimestamp.Store(readTime)
		case QualityMid:
			t.lastReadMidTimestamp.Store(readTime)
		case QualityLow:
			t.lastReadLowTimestamp.Store(readTime)
		}

		tracks := t.base.clientTracks.GetTracks()
		for _, track := range tracks {
			track.push(p, quality)
		}
	}

	switch quality {
	case QualityHigh:
		remoteTrack = NewRemoteTrack(t.base.client, track, onRead)
		t.remoteTrackHigh = remoteTrack
	case QualityMid:
		remoteTrack = NewRemoteTrack(t.base.client, track, onRead)
		t.remoteTrackMid = remoteTrack
	case QualityLow:
		remoteTrack = NewRemoteTrack(t.base.client, track, onRead)
		t.remoteTrackLow = remoteTrack
	default:
		glog.Warning("client: unknown track quality ", track.RID())
		return
	}

	// check if all simulcast tracks are available
	if t.onTrackComplete != nil && t.remoteTrackHigh != nil && t.remoteTrackMid != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}
}

func (t *SimulcastTrack) Subscribe(client *Client) iClientTrack {
	// Create a local track, all our SFU clients will be fed via this track
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.base.codec.RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	ct := &SimulcastClientTrack{
		id:          t.base.id,
		kind:        t.base.kind,
		mimeType:    t.base.codec.MimeType,
		client:      client,
		localTrack:  track,
		remoteTrack: t,
		sourceType:  t.base.sourceType,
	}

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *SimulcastTrack) SetSourceType(sourceType TrackType) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.sourceType = sourceType
}

func (t *SimulcastTrack) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *SimulcastTrack) SourceType() TrackType {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.sourceType
}

func (t *SimulcastTrack) SendPLI(currentTrack *webrtc.TrackRemote) error {
	if currentTrack == nil {
		return nil
	}

	return t.base.client.peerConnection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(currentTrack.SSRC())},
	})
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

		delta := time.Since(time.Unix(0, t.lastReadHighTimestamp.Load()))

		if delta > threshold {
			glog.Warningf("track: remote track %s high is not active, last read was %d ms ago", t.Client().ID, delta.Milliseconds())
			return false
		}

		return true
	case QualityMid:
		if t.remoteTrackMid == nil {
			glog.Warning("track: remote track medium is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadMidTimestamp.Load()))
		if delta > threshold {
			glog.Warningf("track: remote track %s mid is not active, last read was %d ms ago", t.Client().ID, delta.Milliseconds())
			return false
		}

		return true
	case QualityLow:
		if t.remoteTrackLow == nil {
			glog.Warning("track: remote track low is nil")
			return false
		}

		delta := time.Since(time.Unix(0, t.lastReadLowTimestamp.Load()))
		if delta > threshold {
			glog.Warningf("track: remote track %s low is not active, last read was %d ms ago", t.Client().ID, delta.Milliseconds())
			return false
		}

		return true
	}

	return false
}

func (t *SimulcastTrack) sendPLI(quality QualityLevel) {
	switch quality {
	case QualityHigh:
		if err := t.SendPLI(t.remoteTrackHigh.track); err != nil {
			glog.Error("client: error sending PLI ", err)
		}
	case QualityMid:
		if err := t.SendPLI(t.remoteTrackMid.track); err != nil {
			glog.Error("client: error sending PLI ", err)
		}
	case QualityLow:
		if err := t.SendPLI(t.remoteTrackLow.track); err != nil {
			glog.Error("client: error sending PLI ", err)
		}
	}
}

type SubscribeTrackRequest struct {
	ClientID string `json:"client_id"`
	StreamID string `json:"stream_id"`
	TrackID  string `json:"track_id"`
	RID      string `json:"rid"`
}

type TrackList struct {
	tracks map[string]ITrack
	mutex  sync.Mutex
}

func newTrackList() *TrackList {
	return &TrackList{
		tracks: make(map[string]ITrack),
	}
}

func (t *TrackList) Add(track ITrack) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	id := track.ID()
	if _, ok := t.tracks[id]; ok {
		glog.Warning("client: track already added ", id)
		return ErrTrackExists
	}

	t.tracks[id] = track

	return nil
}

func (t *TrackList) Get(ID string) (ITrack, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	if track, ok := t.tracks[ID]; ok {
		return track, nil
	}

	return nil, ErrTrackIsNotExists
}

//nolint:copylocks // This is a read only operation
func (t *TrackList) Remove(ids []string) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for _, id := range ids {
		delete(t.tracks, id)
	}

}

func (t *TrackList) Reset() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.tracks = make(map[string]ITrack)
}

func (t *TrackList) GetTracks() []ITrack {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	tracks := make([]ITrack, 0)
	for _, track := range t.tracks {
		tracks = append(tracks, track)
	}

	return tracks
}

func (t *TrackList) Length() int {
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
