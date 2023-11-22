package sfu

import (
	"context"
	"sync"
)

type clientTrackList struct {
	mu     sync.RWMutex
	tracks []iClientTrack
}

func (l *clientTrackList) Add(track iClientTrack) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	go func() {
		ctx, cancel := context.WithCancel(track.Context())
		defer cancel()
		<-ctx.Done()
		l.remove(track.ID())
	}()

	l.tracks = append(l.tracks, track)
}

func (l *clientTrackList) remove(id string) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	for i, track := range l.tracks {
		if track.ID() == id {
			l.tracks = append(l.tracks[:i], l.tracks[i+1:]...)
			break
		}
	}
}

func (l *clientTrackList) Get(id string) iClientTrack {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, t := range l.tracks {
		if t.ID() == id {
			return t
		}
	}

	return nil
}

func (l *clientTrackList) Length() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.tracks)
}

func (l *clientTrackList) GetTracks() []iClientTrack {
	l.mu.Lock()
	defer l.mu.Unlock()

	tracks := make([]iClientTrack, 0)
	tracks = append(tracks, l.tracks...)

	return tracks
}

func newClientTrackList() *clientTrackList {
	return &clientTrackList{
		mu:     sync.RWMutex{},
		tracks: make([]iClientTrack, 0),
	}
}
