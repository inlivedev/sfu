package sfu

import (
	"context"
	"errors"
	"io"
	"sync"
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
	client       *Client
	isProcessed  bool
	onTrackEnded func(*webrtc.TrackRemote)
	onTrackRead  func(*webrtc.TrackRemote)
	onTrackWrite func(*webrtc.TrackLocalStaticRTP)
	kind         webrtc.RTPCodecType
	sourceType   TrackType // source of the track, can be media or screen
}
type ITrack interface {
	ID() string
	Client() *Client
	IsSimulcast() bool
	IsProcessed() bool
	OnTrackEnded(func(*webrtc.TrackRemote))
	OnTrackRead(func(*webrtc.TrackRemote))
	OnTrackWrite(func(*webrtc.TrackLocalStaticRTP))
	SetSourceType(TrackType)
	SetAsProcessed()
	SourceType() TrackType
	Kind() webrtc.RTPCodecType
	RemoteTrack() *webrtc.TrackRemote
	TotalTracks() int
}

type Track struct {
	base        BaseTrack
	remoteTrack *webrtc.TrackRemote
	localTrack  *webrtc.TrackLocalStaticRTP
}

func NewTrack(client *Client, track *webrtc.TrackRemote) ITrack {
	t := &Track{
		base: BaseTrack{
			id:     track.Msid(),
			client: client,
			kind:   track.Kind(),
		},
		remoteTrack: track,
	}

	t.createLocalTrack()

	setTrackCallback(t)

	return t
}

func (t *Track) createLocalTrack() {
	// Create a local track, all our SFU clients will be fed via this track
	localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrack.Codec().RTPCodecCapability, t.remoteTrack.ID(), t.remoteTrack.StreamID())
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	t.localTrack = localTrack

	rtpBuf := make([]byte, 1450)

	go func() {
		ctxx, cancel := context.WithCancel(t.Client().Context)

		defer cancel()

		for {
			select {
			case <-ctxx.Done():

				return
			default:
				i, _, readErr := t.remoteTrack.Read(rtpBuf)
				if readErr == io.EOF {
					t.base.onTrackEnded(t.remoteTrack)

					return
				}

				t.base.onTrackRead(t.remoteTrack)

				if _, err := localTrack.Write(rtpBuf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					glog.Error("client: local track write error ", err)
					return
				}

				t.base.onTrackWrite(t.localTrack)
			}
		}
	}()
}

func (t *Track) ID() string {
	return t.base.id
}

func (t *Track) Client() *Client {
	return t.base.client
}

func (t *Track) RemoteTrack() *webrtc.TrackRemote {
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

func (t *Track) OnTrackRead(f func(track *webrtc.TrackRemote)) {
	t.base.onTrackRead = f
}

func (t *Track) OnTrackWrite(f func(track *webrtc.TrackLocalStaticRTP)) {
	t.base.onTrackWrite = f
}

func (t *Track) OnTrackEnded(f func(track *webrtc.TrackRemote)) {
	t.base.onTrackEnded = f
}

func (t *Track) TotalTracks() int {
	return 1
}

func (t *Track) Subscribe() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *Track) SetSourceType(sourceType TrackType) {
	t.base.sourceType = sourceType
}

func (t *Track) SetAsProcessed() {
	t.base.isProcessed = true
}

func (t *Track) SendPLI() error {
	return t.base.client.peerConnection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(t.remoteTrack.SSRC())},
	})
}

func (t *Track) GetSourceType() TrackType {
	return t.base.sourceType
}

type ClientLocalTrack struct {
	Client         *Client
	currentQuality QualityLevel
	Track          *webrtc.TrackLocalStaticRTP
	SimulcastTrack *SimulcastTrack
	sequenceNumber uint16
	lastQuality    QualityLevel
}

func (t *ClientLocalTrack) GetQuality() QualityLevel {
	prevQuality := t.currentQuality
	quality := t.Client.GetQuality()

	if prevQuality == quality {
		return quality
	}

	if quality == QualityLow {
		// make sure the track is active before switching to low quality and fallback quality if the track is not active
		if !t.SimulcastTrack.isTrackActive(QualityLow) {
			if !t.SimulcastTrack.isTrackActive(QualityMid) {
				t.switchQuality(QualityHigh)
				return QualityHigh
			}

			t.switchQuality(QualityMid)
			return QualityMid
		}

		t.switchQuality(QualityLow)
		return QualityLow

	}

	if quality == QualityMid {
		if !t.SimulcastTrack.isTrackActive(QualityMid) {
			if !t.SimulcastTrack.isTrackActive(QualityLow) {
				t.switchQuality(QualityHigh)

				return QualityHigh
			}

			t.switchQuality(QualityLow)

			return QualityLow
		}

		t.switchQuality(QualityMid)

		return QualityMid
	}

	if !t.SimulcastTrack.isTrackActive(QualityHigh) {
		if !t.SimulcastTrack.isTrackActive(QualityMid) {
			t.switchQuality(QualityLow)

			return QualityLow
		}

		t.switchQuality(QualityMid)

		return QualityMid
	}

	t.switchQuality(QualityHigh)
	return QualityHigh

}

func (t *ClientLocalTrack) switchQuality(quality QualityLevel) {
	if t.currentQuality == quality {
		return
	}

	t.currentQuality = quality
}

func (t *ClientLocalTrack) sendPLI(quality QualityLevel) {
	switch quality {
	case QualityHigh:
		if err := t.SimulcastTrack.SendPLI(t.SimulcastTrack.remoteTrackHigh); err != nil {
			glog.Error("client: error sending PLI ", err)
		}
	case QualityMid:
		if err := t.SimulcastTrack.SendPLI(t.SimulcastTrack.remoteTrackMid); err != nil {
			glog.Error("client: error sending PLI ", err)
		}
	case QualityLow:
		if err := t.SimulcastTrack.SendPLI(t.SimulcastTrack.remoteTrackLow); err != nil {
			glog.Error("client: error sending PLI ", err)
		}
	}
}

// receive rtp packet from the remote track but only write it to the local track if the quality is the same
// even we have 3 remote track (high, mid, low), we only have 1 local track to write, which remote track packet will be written to the local track is based on the quality
// this is to make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track
func (t *ClientLocalTrack) writeRTP(wg *sync.WaitGroup, rtp *rtp.Packet, quality QualityLevel) {
	var trackQuality QualityLevel

	defer wg.Done()

	if t.Client.peerConnection.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	isKeyframe := IsKeyframe(t.Track.Codec().MimeType, rtp)

	// prevent the packet to be written to the new local track if the packet is not a keyframe
	// this is to avoid broken or froze video on client side
	if !isKeyframe && t.lastQuality == 0 {
		trackQuality = t.GetQuality()
		t.sendPLI(trackQuality)

		return
	}

	if !isKeyframe {
		trackQuality = t.lastQuality
	} else {
		trackQuality = t.GetQuality()
	}

	if trackQuality == quality {
		// make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track

		switch quality {
		case QualityHigh:
			rtp.Timestamp = t.SimulcastTrack.baseTS + ((rtp.Timestamp - t.SimulcastTrack.remoteTrackHighBaseTS) - t.SimulcastTrack.remoteTrackHighBaseTS)
		case QualityMid:
			rtp.Timestamp = t.SimulcastTrack.baseTS + ((rtp.Timestamp - t.SimulcastTrack.remoteTrackMidBaseTS) - t.SimulcastTrack.remoteTrackMidBaseTS)
		case QualityLow:
			rtp.Timestamp = t.SimulcastTrack.baseTS + ((rtp.Timestamp - t.SimulcastTrack.remoteTrackLowBaseTS) - t.SimulcastTrack.remoteTrackLowBaseTS)
		}

		t.sequenceNumber++
		rtp.SequenceNumber = t.sequenceNumber

		if t.lastQuality != quality {
			t.sendPLI(quality)
			t.lastQuality = quality
		}

		if err := t.Track.WriteRTP(rtp); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			glog.Error("client: local track write error ", err)
		}
	}
}

type SimulcastTrack struct {
	base                  BaseTrack
	baseTS                uint32
	context               context.Context
	onTrackComplete       func()
	remoteTrackHigh       *webrtc.TrackRemote
	remoteTrackHighBaseTS uint32
	remoteTrackMid        *webrtc.TrackRemote
	remoteTrackMidBaseTS  uint32
	remoteTrackLow        *webrtc.TrackRemote
	remoteTrackLowBaseTS  uint32
	localTracks           []*ClientLocalTrack
	lastReadHighTimestamp time.Time
	lastReadMidTimestamp  time.Time
	lastReadLowTimestamp  time.Time
}

func NewSimulcastTrack(ctx context.Context, client *Client, track *webrtc.TrackRemote) ITrack {
	t := &SimulcastTrack{
		base: BaseTrack{
			id:     track.Msid(),
			client: client,
			kind:   track.Kind(),
		},
		context: ctx,
	}

	t.AddRemoteTrack(track)
	setTrackCallback(t)

	return t
}

func (t *SimulcastTrack) OnTrackRead(f func(track *webrtc.TrackRemote)) {
	t.base.onTrackRead = f
}

func (t *SimulcastTrack) OnTrackWrite(f func(track *webrtc.TrackLocalStaticRTP)) {
	t.base.onTrackWrite = f
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
	quality := RIDToQuality(track.RID())
	switch quality {
	case QualityHigh:
		t.remoteTrackHigh = track
	case QualityMid:
		t.remoteTrackMid = track
	case QualityLow:
		t.remoteTrackLow = track
	default:
		glog.Warning("client: unknown track quality ", track.RID())

		return
	}

	go func() {
		ctxx, cancel := context.WithCancel(t.context)

		defer cancel()

		for {
			select {
			case <-ctxx.Done():

				return
			default:
				rtp, _, readErr := track.ReadRTP()
				if readErr == io.EOF {
					if t.base.onTrackEnded != nil {
						go t.base.onTrackEnded(track)
					}

					return
				}

				// set the base timestamp for the track if it is not set yet
				if t.baseTS == 0 {
					t.baseTS = rtp.Timestamp
				}

				if quality == QualityHigh && t.remoteTrackHighBaseTS == 0 {
					t.remoteTrackHighBaseTS = rtp.Timestamp
				} else if quality == QualityMid && t.remoteTrackMidBaseTS == 0 {
					t.remoteTrackMidBaseTS = rtp.Timestamp
				} else if quality == QualityLow && t.remoteTrackLowBaseTS == 0 {
					t.remoteTrackLowBaseTS = rtp.Timestamp
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

				t.onRTPRead(rtp, quality)

				if t.base.onTrackRead != nil {
					t.base.onTrackRead(track)
				}

			}
		}
	}()

	// check if all simulcast tracks are available
	if t.onTrackComplete != nil && t.remoteTrackHigh != nil && t.remoteTrackMid != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}
}

func (t *SimulcastTrack) onRTPRead(packet *rtp.Packet, quality QualityLevel) {
	isWritten := false
	wg := &sync.WaitGroup{}
	wg.Add(len(t.localTracks))

	for _, localTrack := range t.localTracks {
		go localTrack.writeRTP(wg, packet, quality)
	}

	tracksCount := len(t.localTracks)

	if tracksCount != 0 {

		c := make(chan bool)
		go func() {
			defer close(c)
			wg.Wait()
		}()

		ctx, cancel := context.WithCancel(t.context)
		defer cancel()
		select {
		case <-ctx.Done():
		case <-c:
			isWritten = true
		}

	}

	if isWritten {
		// TODO: check this parameter, not sure if it is correct
		go t.base.onTrackWrite(t.localTracks[0].Track)
	}
}

func (t *SimulcastTrack) Subscribe(client *Client) *webrtc.TrackLocalStaticRTP {
	// Create a local track, all our SFU clients will be fed via this track
	remoteTrack := t.RemoteTrack()
	localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, remoteTrack.ID(), remoteTrack.StreamID())
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	t.localTracks = append(t.localTracks, &ClientLocalTrack{
		Client:         client,
		Track:          localTrack,
		SimulcastTrack: t,
	})

	return localTrack
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

func (t *SimulcastTrack) RemoteTrack() *webrtc.TrackRemote {
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
			glog.Warningf("track: remote track %s high is not active, last read was %d ms ago", t.Client().ID, delta)
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
			glog.Warningf("track: remote track %s mid is not active, last read was %d ms ago", t.Client().ID, delta)
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
			glog.Warningf("track: remote track %s low is not active, last read was %d ms ago", t.Client().ID, delta)
			return false
		}

		return true
	}

	return false
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

	track.OnTrackRead(func(remoteTrack *webrtc.TrackRemote) {
		// this will update the bandwidth stats everytime we receive packet from the remote track
		go client.updateReceiverStats(remoteTrack)
	})

	track.OnTrackWrite(func(track *webrtc.TrackLocalStaticRTP) {
		// this can be use to update the bandwidth stats everytime we send packet to the remote peer
		// TODO: figure out the best use case for this callback

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
