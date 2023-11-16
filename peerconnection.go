package sfu

import (
	"sync"

	"github.com/pion/webrtc/v3"
)

type PeerConnection struct {
	mu sync.Mutex
	pc *webrtc.PeerConnection
}

func newPeerConnection(pc *webrtc.PeerConnection) *PeerConnection {
	return &PeerConnection{
		mu: sync.Mutex{},
		pc: pc,
	}
}

func (p *PeerConnection) PC() *webrtc.PeerConnection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pc
}

func (p *PeerConnection) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.pc.Close()
}

func (p *PeerConnection) AddTrack(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.pc.AddTrack(track)
}

func (p *PeerConnection) RemoveTrack(sender *webrtc.RTPSender) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.pc.RemoveTrack(sender)
}
