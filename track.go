package sfu

import (
	"errors"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/interceptor/pkg/stats"
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
	client       *Client
	isProcessed  bool
	onTrackEnded func(*webrtc.TrackRemote)
	onTrackRead  func(ITrack, *webrtc.TrackRemote)
	kind         webrtc.RTPCodecType
	sourceType   TrackType // source of the track, can be media or screen
}

type ITrack interface {
	ID() string
	Client() *Client
	IsSimulcast() bool
	IsProcessed() bool
	OnTrackEnded(func(*webrtc.TrackRemote))
	OnTrackRead(func(ITrack, *webrtc.TrackRemote))
	SetSourceType(TrackType)
	UpdateStats(*webrtc.TrackRemote, stats.Stats)
	SetAsProcessed()
	SourceType() TrackType
	Kind() webrtc.RTPCodecType
	RemoteTrack() *RemoteTrack
	TotalTracks() int
}

type Track struct {
	base        BaseTrack
	remoteTrack *RemoteTrack
	localTracks []*ClientTrack
}

func NewTrack(client *Client, track *webrtc.TrackRemote) ITrack {
	t := &Track{
		base: BaseTrack{
			id:     track.Msid(),
			client: client,
			kind:   track.Kind(),
		},
		remoteTrack: NewRemoteTrack(client.Context(), track),
	}

	t.remoteTrack.onRead = t.onRead

	t.remoteTrack.onEnded = t.onEnded

	setTrackCallback(t)

	return t
}

func (t *Track) onRead(rtp *rtp.Packet) {
	t.base.onTrackRead(t, t.remoteTrack.track)
	for _, localTrack := range t.localTracks {
		localTrack.push(t.ID(), rtp)
	}
}

func (t *Track) onEnded() {
	t.base.onTrackEnded(t.remoteTrack.track)
}

func (t *Track) createLocalTrack() *webrtc.TrackLocalStaticRTP {
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrack.track.Codec().RTPCodecCapability, t.remoteTrack.track.ID(), t.remoteTrack.track.StreamID())
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	return track
}

func (t *Track) ID() string {
	return t.base.id
}

func (t *Track) Client() *Client {
	return t.base.client
}

func (t *Track) RemoteTrack() *RemoteTrack {
	return t.remoteTrack
}

func (t *Track) SourceType() TrackType {
	return t.base.sourceType
}

func (t *Track) IsSimulcast() bool {
	return false
}

func (t *Track) IsProcessed() bool {
	return t.base.isProcessed
}

func (t *Track) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *Track) OnTrackRead(f func(ITrack, *webrtc.TrackRemote)) {
	t.base.onTrackRead = f
}

func (t *Track) OnTrackEnded(f func(track *webrtc.TrackRemote)) {
	t.base.onTrackEnded = f
}

func (t *Track) TotalTracks() int {
	return 1
}

func (t *Track) Subscribe() *webrtc.TrackLocalStaticRTP {
	clientTrack := &ClientTrack{
		client:      t.Client(),
		localTrack:  t.createLocalTrack(),
		remoteTrack: t.remoteTrack,
		sourceType:  t.base.sourceType,
	}

	t.localTracks = append(t.localTracks, clientTrack)

	return clientTrack.localTrack
}

func (t *Track) SetSourceType(sourceType TrackType) {
	t.base.sourceType = sourceType
}

func (t *Track) SetAsProcessed() {
	t.base.isProcessed = true
}

func (t *Track) SendPLI() error {
	return t.base.client.peerConnection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(t.remoteTrack.track.SSRC())},
	})
}

func (t *Track) GetSourceType() TrackType {
	return t.base.sourceType
}

func (t *Track) UpdateStats(track *webrtc.TrackRemote, s stats.Stats) {
	t.remoteTrack.updateStats(s)
}

type ClientTrack struct {
	client      *Client
	localTrack  *webrtc.TrackLocalStaticRTP
	remoteTrack *RemoteTrack
	sourceType  TrackType
}

func (t *ClientTrack) push(trackID string, rtp *rtp.Packet) {
	if t.client.peerConnection.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	t.localTrack.WriteRTP(rtp)
}

type SimulcastClientTrack struct {
	client         *Client
	localTrack     *webrtc.TrackLocalStaticRTP
	remoteTrack    *SimulcastTrack
	sequenceNumber uint16
	lastQuality    QualityLevel
	lastTimestamp  uint32
	sourceType     TrackType
}

func (t *SimulcastClientTrack) GetAllowedQuality(kind webrtc.RTPCodecType) QualityLevel {
	var estimatedQuality QualityLevel
	var currentHighBitrate, currentMidBitrate, currentLowBitrate uint32

	if kind == webrtc.RTPCodecTypeAudio {
		return QualityHigh
	}

	_, maxAllowedVideoBitrate := t.client.getMaxPerTrackQuality()

	if t.remoteTrack.isTrackActive(QualityHigh) {
		currentHighBitrate = t.remoteTrack.remoteTrackHigh.GetCurrentBitrate()
	} else {
		currentHighBitrate = highBitrate
	}

	if t.remoteTrack.isTrackActive(QualityMid) {
		currentMidBitrate = t.remoteTrack.remoteTrackMid.GetCurrentBitrate()
	} else {
		currentMidBitrate = midBitrate
	}

	if t.remoteTrack.isTrackActive(QualityLow) {
		currentLowBitrate = t.remoteTrack.remoteTrackLow.GetCurrentBitrate()
	} else {
		currentLowBitrate = lowBitrate
	}

	if maxAllowedVideoBitrate > currentHighBitrate {
		estimatedQuality = QualityHigh
	} else if maxAllowedVideoBitrate < currentHighBitrate && maxAllowedVideoBitrate > currentMidBitrate {
		estimatedQuality = QualityMid
	} else if maxAllowedVideoBitrate < currentMidBitrate && maxAllowedVideoBitrate > currentLowBitrate {
		estimatedQuality = QualityLow
	} else {
		estimatedQuality = QualityNone
	}

	if t.client.quality != 0 && estimatedQuality > t.client.quality {
		return t.client.quality
	}

	return estimatedQuality

}

func (t *SimulcastClientTrack) GetQuality() QualityLevel {
	track := t.remoteTrack

	var qualityFinal QualityLevel = QualityNone

	quality := t.GetAllowedQuality(track.Kind())

	if quality == QualityHigh {
		if track.isTrackActive(QualityHigh) {
			qualityFinal = QualityHigh
		} else if track.isTrackActive(QualityMid) {
			qualityFinal = QualityMid
		} else if track.isTrackActive(QualityLow) {
			qualityFinal = QualityLow
		}
	} else if quality == QualityMid {
		if track.isTrackActive(QualityMid) {
			qualityFinal = QualityMid
		} else if track.isTrackActive(QualityLow) {
			qualityFinal = QualityLow
		}
	} else if quality == QualityLow && track.isTrackActive(QualityLow) {
		qualityFinal = QualityLow
	}

	return qualityFinal

}

// receive rtp packet from the remote track but only write it to the local track if the quality is the same
// even we have 3 remote track (high, mid, low), we only have 1 local track to write, which remote track packet will be written to the local track is based on the quality
// this is to make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track
func (t *SimulcastClientTrack) push(rtp *rtp.Packet, quality QualityLevel) {

	var trackQuality QualityLevel

	if t.client.peerConnection.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	isKeyframe := IsKeyframe(t.remoteTrack.RemoteTrack().track.Codec().MimeType, rtp)

	// prevent the packet to be written to the new local track if the packet is not a keyframe
	// this is to avoid broken or froze video on client side
	if !isKeyframe && t.lastQuality == 0 {
		trackQuality = t.GetQuality()
		t.remoteTrack.sendPLI(trackQuality)

		return
	}

	if t.lastQuality == 0 {
		trackQuality = QualityLow
	} else if isKeyframe && t.lastTimestamp != rtp.Timestamp {
		trackQuality = t.GetQuality()
	} else {
		trackQuality = t.lastQuality
	}

	if trackQuality == quality {
		// set the last processed packet timestamp to identify if is begining of the new frame
		t.lastTimestamp = rtp.Timestamp
		// make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track

		// credit to https://github.com/k0nserv for helping me with this on Pion Slack channel
		switch quality {
		case QualityHigh:
			rtp.Timestamp = t.remoteTrack.baseTS + ((rtp.Timestamp - t.remoteTrack.remoteTrackHighBaseTS) - t.remoteTrack.remoteTrackHighBaseTS)
		case QualityMid:
			rtp.Timestamp = t.remoteTrack.baseTS + ((rtp.Timestamp - t.remoteTrack.remoteTrackMidBaseTS) - t.remoteTrack.remoteTrackMidBaseTS)
		case QualityLow:
			rtp.Timestamp = t.remoteTrack.baseTS + ((rtp.Timestamp - t.remoteTrack.remoteTrackLowBaseTS) - t.remoteTrack.remoteTrackLowBaseTS)
		}

		t.sequenceNumber++
		rtp.SequenceNumber = t.sequenceNumber

		if t.lastQuality != quality {
			t.remoteTrack.sendPLI(quality)
			t.lastQuality = quality
		}

		if err := t.localTrack.WriteRTP(rtp); err != nil {
			glog.Error("track: error on write rtp", err)
		}
	}
}

type SimulcastTrack struct {
	base                  BaseTrack
	baseTS                uint32
	onTrackComplete       func()
	remoteTrackHigh       *RemoteTrack
	remoteTrackHighBaseTS uint32
	remoteTrackMid        *RemoteTrack
	remoteTrackMidBaseTS  uint32
	remoteTrackLow        *RemoteTrack
	remoteTrackLowBaseTS  uint32
	localTracks           []*SimulcastClientTrack
	lastReadHighTimestamp time.Time
	lastReadMidTimestamp  time.Time
	lastReadLowTimestamp  time.Time
}

func NewSimulcastTrack(client *Client, track *webrtc.TrackRemote) ITrack {
	t := &SimulcastTrack{
		base: BaseTrack{
			id:     track.Msid(),
			client: client,
			kind:   track.Kind(),
		},
	}

	t.AddRemoteTrack(track)
	setTrackCallback(t)

	return t
}

func (t *SimulcastTrack) OnTrackRead(f func(track ITrack, trackRemote *webrtc.TrackRemote)) {
	t.base.onTrackRead = f
}

func (t *SimulcastTrack) OnTrackEnded(f func(track *webrtc.TrackRemote)) {
	t.base.onTrackEnded = f
}

func (t *SimulcastTrack) OnTrackComplete(f func()) {
	t.onTrackComplete = f
}

// TODO: this is contain multiple tracks, there is a possibility remote track high is not available yet
func (t *SimulcastTrack) ID() string {
	return t.base.id
}

func (t *SimulcastTrack) Client() *Client {
	return t.base.client
}

func (t *SimulcastTrack) IsSimulcast() bool {
	return true
}

func (t *SimulcastTrack) IsProcessed() bool {
	return t.base.isProcessed
}

func (t *SimulcastTrack) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *SimulcastTrack) AddRemoteTrack(track *webrtc.TrackRemote) {
	var remoteTrack *RemoteTrack

	quality := RIDToQuality(track.RID())
	switch quality {
	case QualityHigh:
		remoteTrack = NewRemoteTrack(t.base.client.Context(), track)
		t.remoteTrackHigh = remoteTrack
	case QualityMid:
		remoteTrack = NewRemoteTrack(t.base.client.Context(), track)
		t.remoteTrackMid = remoteTrack
	case QualityLow:
		remoteTrack = NewRemoteTrack(t.base.client.Context(), track)
		t.remoteTrackLow = remoteTrack
	default:
		glog.Warning("client: unknown track quality ", track.RID())
		return
	}

	remoteTrack.onRead = func(p *rtp.Packet) {
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

		readTime := time.Now()

		switch quality {
		case QualityHigh:
			t.lastReadHighTimestamp = readTime
		case QualityMid:
			t.lastReadMidTimestamp = readTime
		case QualityLow:
			t.lastReadLowTimestamp = readTime
		}

		t.onRTPRead(p, quality)

		if t.base.onTrackRead != nil {
			t.base.onTrackRead(t, track)
		}
	}

	remoteTrack.onEnded = func() {
		go t.base.onTrackEnded(track)
	}

	// check if all simulcast tracks are available
	if t.onTrackComplete != nil && t.remoteTrackHigh != nil && t.remoteTrackMid != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}
}

func (t *SimulcastTrack) onRTPRead(packet *rtp.Packet, quality QualityLevel) {
	for _, localTrack := range t.localTracks {
		localTrack.push(packet, quality)
	}
}

func (t *SimulcastTrack) Subscribe(client *Client) *webrtc.TrackLocalStaticRTP {
	// Create a local track, all our SFU clients will be fed via this track
	remoteTrack := t.RemoteTrack().track
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, remoteTrack.ID(), remoteTrack.StreamID())
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	t.localTracks = append(t.localTracks, &SimulcastClientTrack{
		client:      client,
		localTrack:  track,
		remoteTrack: t,
		sourceType:  t.base.sourceType,
	})

	return track
}

func (t *SimulcastTrack) SetSourceType(sourceType TrackType) {
	t.base.sourceType = sourceType
}

func (t *SimulcastTrack) SetAsProcessed() {
	t.base.isProcessed = true
}

func (t *SimulcastTrack) SourceType() TrackType {
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

func (t *SimulcastTrack) RemoteTrack() *RemoteTrack {
	if t.remoteTrackHigh != nil {
		return t.remoteTrackHigh
	}

	if t.remoteTrackMid != nil {
		return t.remoteTrackMid
	}

	return t.remoteTrackLow
}

func (t *SimulcastTrack) TotalTracks() int {
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
	// set max active track threshold to 500ms
	threshold := time.Duration(500) * time.Millisecond

	switch quality {
	case QualityHigh:
		if t.remoteTrackHigh == nil {
			glog.Warning("track: remote track high is nil")
			return false
		}

		delta := time.Since(t.lastReadHighTimestamp)

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

		delta := time.Since(t.lastReadMidTimestamp)
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

		delta := time.Since(t.lastReadLowTimestamp)
		if delta > threshold {
			glog.Warningf("track: remote track %s low is not active, last read was %d ms ago", t.Client().ID, delta.Milliseconds())
			return false
		}

		return true
	}

	return false
}

func (t *SimulcastTrack) UpdateStats(track *webrtc.TrackRemote, s stats.Stats) {
	quality := RIDToQuality(track.RID())
	switch quality {
	case QualityHigh:
		t.remoteTrackHigh.updateStats(s)
	case QualityMid:
		t.remoteTrackMid.updateStats(s)
	case QualityLow:
		t.remoteTrackLow.updateStats(s)
	}
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
func (t *TrackList) Remove(ID string) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	delete(t.tracks, ID)

}

func (t *TrackList) Reset() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.tracks = make(map[string]ITrack)
}

func (t *TrackList) GetTracks() map[string]ITrack {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	return t.tracks
}

func (t *TrackList) Length() int {
	return len(t.tracks)
}

func setTrackCallback(track ITrack) {
	client := track.Client()
	track.OnTrackEnded(func(track *webrtc.TrackRemote) {
		client.tracks.Remove(track.ID())
		trackIDs := make([]string, 0)
		trackIDs = append(trackIDs, track.ID())

		client.sfu.removeTracks(trackIDs)
	})

	track.OnTrackRead(func(t ITrack, remoteTrack *webrtc.TrackRemote) {
		// this will update the bandwidth stats everytime we receive packet from the remote track
		go client.updateReceiverStats(t, remoteTrack)
	})
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
