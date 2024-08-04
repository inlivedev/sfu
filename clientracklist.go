package sfu

import (
	"sync"
)

type clientTrackList struct {
	mu     sync.RWMutex
	tracks []iClientTrack
}

func (l *clientTrackList) Add(track iClientTrack) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// TODO: change to non go routine
	track.OnEnded(func() {
		l.remove(track.ID())
	})

	l.tracks = append(l.tracks, track)
}

func (l *clientTrackList) remove(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, track := range l.tracks {
		if track.ID() == id {
			l.tracks = append(l.tracks[:i], l.tracks[i+1:]...)
			break
		}
	}
}

func (l *clientTrackList) Get(id string) iClientTrack {
	l.mu.RLock()
	defer l.mu.RUnlock()

	for _, t := range l.tracks {
		if t.ID() == id {
			return t
		}
	}

	return nil
}

func (l *clientTrackList) Length() int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return len(l.tracks)
}

func (l *clientTrackList) GetTracks() []iClientTrack {
	l.mu.RLock()
	defer l.mu.RUnlock()
	clientTracks := make([]iClientTrack, len(l.tracks))
	copy(clientTracks, l.tracks)
	return clientTracks
}

func newClientTrackList() *clientTrackList {
	return &clientTrackList{
		mu:     sync.RWMutex{},
		tracks: make([]iClientTrack, 0),
	}
}
