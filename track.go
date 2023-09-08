package sfu

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

const (
	TrackTypeMedia  = "media"
	TrackTypeScreen = "screen"

	QualityHigh   = "high"
	QualityMedium = "mid"
	QualityLow    = "low"
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

	setTrackCallback(t)

	return t
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

func (t *Track) Subscribe(ctx context.Context) (*webrtc.TrackLocalStaticRTP, error) {
	if t.localTrack != nil {
		return t.localTrack, nil
	}

	// Create a local track, all our SFU clients will be fed via this track
	localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrack.Codec().RTPCodecCapability, t.remoteTrack.ID(), t.remoteTrack.StreamID())
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	t.localTrack = localTrack

	rtpBuf := make([]byte, 1450)

	go func() {
		ctxx, cancel := context.WithCancel(ctx)

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

				// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
				if _, err := localTrack.Write(rtpBuf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					glog.Error("client: local track write error ", err)
				}

				t.base.onTrackWrite(t.localTrack)
			}
		}
	}()

	return localTrack, nil
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
	Client           *Client
	Track            *webrtc.TrackLocalStaticRTP
	SimulcastTrack   *SimulcastTrack
	sequenceNumber   uint16
	lastTimestamp    uint32
	currentTimestamp uint32
}

func (t *ClientLocalTrack) GetQuality() string {
	prevQuality := t.SimulcastTrack.currentQuality
	quality := t.Client.GetQuality()

	if prevQuality == quality {
		return quality
	}

	if quality == QualityHigh {
		if t.SimulcastTrack.remoteTrackHigh == nil {
			if t.SimulcastTrack.remoteTrackMedium == nil {
				t.SimulcastTrack.SwitchQuality(QualityLow)

				return QualityLow
			}

			t.SimulcastTrack.SwitchQuality(QualityMedium)

			return QualityMedium
		}

		return QualityHigh
	} else if quality == QualityMedium {
		if t.SimulcastTrack.remoteTrackMedium == nil {
			if t.SimulcastTrack.remoteTrackLow == nil {
				t.SimulcastTrack.SwitchQuality(QualityHigh)

				return QualityHigh
			}

			t.SimulcastTrack.SwitchQuality(QualityLow)

			return QualityLow
		}

		return QualityMedium
	}

	if t.SimulcastTrack.remoteTrackLow == nil {
		if t.SimulcastTrack.remoteTrackMedium == nil {
			return QualityHigh
		}

		return QualityMedium
	}

	return QualityLow

}

type SimulcastTrack struct {
	base              BaseTrack
	context           context.Context
	onTrackComplete   func()
	remoteTrackHigh   *webrtc.TrackRemote
	remoteTrackMedium *webrtc.TrackRemote
	remoteTrackLow    *webrtc.TrackRemote
	currentQuality    string
	localTracks       []*ClientLocalTrack
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
	switch track.RID() {
	case QualityHigh:
		t.remoteTrackHigh = track
	case QualityMedium:
		t.remoteTrackMedium = track
	case QualityLow:
		t.remoteTrackLow = track
	default:
		glog.Warning("client: unknown track quality ", track.RID())
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
				if readErr == io.EOF && t.base.onTrackEnded != nil {
					go t.base.onTrackEnded(track)

					return
				}

				if t.base.onTrackRead != nil {
					go t.base.onTrackRead(track)
				}

				t.onRTPRead(rtp, track.RID())

			}
		}
	}()

	// check if all simulcast tracks are available
	if t.onTrackComplete != nil && t.remoteTrackHigh != nil && t.remoteTrackMedium != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}
}
func (t *SimulcastTrack) writeRTP(writtenChan chan bool, track *ClientLocalTrack, rtp *rtp.Packet, RID string) {
	quality := track.GetQuality()
	written := false

	if RID == quality {
		// // make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track
		oldTimestamp := rtp.Timestamp

		if track.lastTimestamp != 0 {
			delta := rtp.Timestamp - track.lastTimestamp
			track.currentTimestamp += delta

			rtp.Timestamp = track.currentTimestamp
		}

		glog.Infof("simulcast: client: %s RID: %s timestamp: %v sequence: ", t.base.client.ID, quality, rtp.Timestamp, rtp.SequenceNumber)

		track.lastTimestamp = oldTimestamp

		// localTrack is new, we need to set the sequence number
		if track.sequenceNumber == 0 {
			track.sequenceNumber = rtp.SequenceNumber
		} else {
			rtp.SequenceNumber = track.sequenceNumber
		}

		// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
		if err := track.Track.WriteRTP(rtp); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			glog.Error("client: local track write error ", err)
		}

		track.sequenceNumber++

		written = true
	}

	writtenChan <- written
}

func (t *SimulcastTrack) onRTPRead(packet *rtp.Packet, RID string) {
	isWritten := false
	writtenChan := make(chan bool)

	for _, localTrack := range t.localTracks {
		go t.writeRTP(writtenChan, localTrack, packet, RID)
	}

	tracksCount := len(t.localTracks)

	if tracksCount != 0 {
		trackDone := 0
	Loop:
		for {
			ctx, cancel := context.WithCancel(t.context)
			defer cancel()
			select {
			case <-ctx.Done():
				break Loop
			case written := <-writtenChan:
				trackDone++
				if trackDone == tracksCount {
					isWritten = written
					break Loop
				}
			}
		}
	}

	if isWritten {
		// TODO: check this parameter, not sure if it is correct
		go t.base.onTrackWrite(t.localTracks[0].Track)
	}
}

func (t *SimulcastTrack) Subscribe(client *Client) (*webrtc.TrackLocalStaticRTP, error) {
	// Create a local track, all our SFU clients will be fed via this track
	localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrackHigh.Codec().RTPCodecCapability, t.remoteTrackHigh.ID(), t.remoteTrackHigh.StreamID())
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	t.localTracks = append(t.localTracks, &ClientLocalTrack{
		Client:         client,
		Track:          localTrack,
		SimulcastTrack: t,
	})

	return localTrack, nil
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
	return t.base.client.peerConnection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(currentTrack.SSRC())},
	})
}

func (t *SimulcastTrack) SwitchQuality(quality string) {
	t.currentQuality = quality

	switch quality {
	case QualityHigh:
		t.SendPLI(t.remoteTrackHigh)
	case QualityMedium:
		t.SendPLI(t.remoteTrackMedium)
	case QualityLow:
		t.SendPLI(t.remoteTrackLow)
	}
}

func (t *SimulcastTrack) RemoteTrack() *webrtc.TrackRemote {
	return t.remoteTrackHigh
}

func (t *SimulcastTrack) TotalTracks() int {
	total := 0
	if t.remoteTrackHigh != nil {
		total++
	}

	if t.remoteTrackMedium != nil {
		total++
	}

	if t.remoteTrackLow != nil {
		total++
	}

	return total
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
		tracks := make([]ITrack, 0)
		tracks = append(tracks, NewTrack(client, track))

		client.sfu.removeTracks(tracks)
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
