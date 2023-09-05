package sfu

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

const (
	TrackTypeMedia  = "media"
	TrackTypeScreen = "screen"

	QualityHigh   = "high"
	QualityMedium = "medium"
	QualityLow    = "low"
)

var (
	ErrTrackExists      = errors.New("client: error track already exists")
	ErrTrackIsNotExists = errors.New("client: error track is not exists")
)

type TrackType string

type ITrack interface {
	ID() string
	ClientID() string
	IsSimulcast() bool
	OnTrackEnded(func(*webrtc.TrackRemote))
	OnTrackRead(func(*webrtc.TrackRemote))
	OnTrackWrite(func(*webrtc.TrackLocalStaticRTP))
	SetSourceType(TrackType)
	SourceType() TrackType
	RemoteTrack() *webrtc.TrackRemote
}

type Track struct {
	clientID     string
	onTrackEnded func(*webrtc.TrackRemote)
	onTrackRead  func(*webrtc.TrackRemote)
	onTrackWrite func(*webrtc.TrackLocalStaticRTP)
	remoteTrack  *webrtc.TrackRemote
	localTrack   *webrtc.TrackLocalStaticRTP
	sourceType   TrackType // source of the track, can be media or screen
}

func NewTrack(clientID string, track *webrtc.TrackRemote) ITrack {
	return &Track{
		clientID:    clientID,
		remoteTrack: track,
	}
}

func (t *Track) ID() string {
	return t.remoteTrack.Msid()
}

func (t *Track) ClientID() string {
	return t.clientID
}

func (t *Track) RemoteTrack() *webrtc.TrackRemote {
	return t.remoteTrack
}

func (t *Track) SourceType() TrackType {
	return t.sourceType
}

func (t *Track) IsSimulcast() bool {
	return false
}

func (t *Track) OnTrackRead(f func(track *webrtc.TrackRemote)) {
	t.onTrackRead = f
}

func (t *Track) OnTrackWrite(f func(track *webrtc.TrackLocalStaticRTP)) {
	t.onTrackWrite = f
}

func (t *Track) OnTrackEnded(f func(track *webrtc.TrackRemote)) {
	t.onTrackEnded = f
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
					t.onTrackEnded(t.remoteTrack)

					return
				}

				t.onTrackRead(t.remoteTrack)

				// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
				if _, err := localTrack.Write(rtpBuf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					glog.Error("client: local track write error ", err)
				}

				t.onTrackWrite(t.localTrack)
			}
		}
	}()

	return localTrack, nil
}

func (t *Track) SetSourceType(sourceType TrackType) {
	t.sourceType = sourceType
}

func (t *Track) GetSourceType() TrackType {
	return t.sourceType
}

type ClientLocalTrack struct {
	Client         *Client
	Track          *webrtc.TrackLocalStaticRTP
	sequenceNumber uint16
	lastTimestamp  uint32
}

type SimulcastTrack struct {
	context           context.Context
	clientID          string
	onTrackEnded      func(*webrtc.TrackRemote)
	onTrackRead       func(*webrtc.TrackRemote)
	onTrackWrite      func(*webrtc.TrackLocalStaticRTP)
	onTrackComplete   func()
	remoteTrackHigh   *webrtc.TrackRemote
	remoteTrackMedium *webrtc.TrackRemote
	remoteTrackLow    *webrtc.TrackRemote
	localTracks       []ClientLocalTrack
	sourceType        TrackType
}

func NewSimulcastTrack(clientID string, track *webrtc.TrackRemote) ITrack {
	t := &SimulcastTrack{
		clientID: clientID,
	}
	t.AddRemoteTrack(track)
	return t
}

func (t *SimulcastTrack) OnTrackRead(f func(track *webrtc.TrackRemote)) {
	t.onTrackRead = f
}

func (t *SimulcastTrack) OnTrackWrite(f func(track *webrtc.TrackLocalStaticRTP)) {
	t.onTrackWrite = f
}

func (t *SimulcastTrack) OnTrackEnded(f func(track *webrtc.TrackRemote)) {
	t.onTrackEnded = f
}

func (t *SimulcastTrack) ID() string {
	return t.remoteTrackHigh.Msid()
}

func (t *SimulcastTrack) ClientID() string {
	return t.clientID
}

func (t *SimulcastTrack) IsSimulcast() bool {
	return true
}

func (t *SimulcastTrack) AddRemoteTrack(track *webrtc.TrackRemote) {
	switch track.RID() {
	case QualityHigh:
		t.remoteTrackHigh = track
	case QualityMedium:
		t.remoteTrackMedium = track
	case QualityLow:
		t.remoteTrackLow = track
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
					t.onTrackEnded(track)

					return
				}

				t.onTrackRead(track)
				t.onRTPRead(rtp, track.RID())
			}
		}
	}()

	// check if all simulcast tracks are available
	if t.remoteTrackHigh != nil && t.remoteTrackMedium != nil && t.remoteTrackLow != nil {
		t.onTrackComplete()
	}
}

func (t *SimulcastTrack) onRTPRead(packet *rtp.Packet, RID string) {
	isWritten := false
	for _, localTrack := range t.localTracks {
		if RID == localTrack.Client.GetQuality() {
			// make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track
			if localTrack.lastTimestamp != 0 {
				delta := packet.Timestamp - localTrack.lastTimestamp
				packet.Timestamp = localTrack.lastTimestamp + delta
			}

			localTrack.lastTimestamp = packet.Timestamp
			packet.SequenceNumber = localTrack.sequenceNumber
			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			if err := localTrack.Track.WriteRTP(packet); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				glog.Error("client: local track write error ", err)
			}
			localTrack.sequenceNumber++
			isWritten = true
		}
	}

	if !isWritten {
		// TODO: check this parameter, not sure if it is correct
		t.onTrackWrite(t.localTracks[0].Track)
	}
}

func (t *SimulcastTrack) Subscribe(client *Client) (*webrtc.TrackLocalStaticRTP, error) {
	// Create a local track, all our SFU clients will be fed via this track
	localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrackHigh.Codec().RTPCodecCapability, t.remoteTrackHigh.ID(), t.remoteTrackHigh.StreamID())
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	t.localTracks = append(t.localTracks, ClientLocalTrack{
		Client: client,
		Track:  localTrack,
	})

	return localTrack, nil
}

func (t *SimulcastTrack) SetSourceType(sourceType TrackType) {
	t.sourceType = sourceType
}

func (t *SimulcastTrack) SourceType() TrackType {
	return t.sourceType
}

func (t *SimulcastTrack) RemoteTrack() *webrtc.TrackRemote {
	return t.remoteTrackHigh
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
