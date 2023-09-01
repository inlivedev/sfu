package sfu

import (
	"errors"
	"fmt"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
)

const (
	TrackTypeMedia  = "media"
	TrackTypeScreen = "screen"
)

var (
	ErrTrackExists = errors.New("client: error track already exists")
)

type TrackType string

type Track struct {
	ClientID       string
	LocalStaticRTP *webrtc.TrackLocalStaticRTP
	SourceType     TrackType // source of the track, can be media or screen
}

func (t *Track) ID() string {
	return fmt.Sprintf("%s-%s", t.LocalStaticRTP.StreamID(), t.LocalStaticRTP.ID())
}

type SubscribeTrackRequest struct {
	ClientID string `json:"client_id"`
	StreamID string `json:"stream_id"`
	TrackID  string `json:"track_id"`
}

type TrackList struct {
	tracks map[string]*Track
	mutex  sync.Mutex
}

func newTrackList() *TrackList {
	return &TrackList{
		tracks: make(map[string]*Track),
	}
}

func (t *TrackList) Add(track *Track) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	id := track.ID()
	if _, ok := t.tracks[id]; ok {
		glog.Warning("client: track already added ", track.ClientID, track.LocalStaticRTP.StreamID(), track.LocalStaticRTP.ID())
		return ErrTrackExists
	}

	t.tracks[id] = track

	return nil
}

//nolint:copylocks // This is a read only operation
func (t *TrackList) Remove(track *webrtc.TrackLocalStaticRTP) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	id := track.StreamID() + "-" + track.ID()

	delete(t.tracks, id)
}

func (t *TrackList) Reset() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.tracks = make(map[string]*Track)
}

func (t *TrackList) GetTracks() map[string]*Track {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	return t.tracks
}

func (t *TrackList) Length() int {
	return len(t.tracks)
}
