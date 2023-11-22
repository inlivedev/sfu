package sfu

import (
	"sync"
	"sync/atomic"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type iClientTrack interface {
	push(rtp rtp.Packet, quality QualityLevel)
	ID() string
	Kind() webrtc.RTPCodecType
	LocalTrack() *webrtc.TrackLocalStaticRTP
	IsScreen() bool
	IsSimulcast() bool
	IsScaleable() bool
	SetSourceType(TrackType)
	OnTrackEnded(func())
	onTrackEnded()
	Client() *Client
	RequestPLI()
	SetMaxQuality(quality QualityLevel)
	MaxQuality() QualityLevel
}

type clientTrack struct {
	id                    string
	mu                    sync.RWMutex
	client                *Client
	kind                  webrtc.RTPCodecType
	mimeType              string
	localTrack            *webrtc.TrackLocalStaticRTP
	remoteTrack           *remoteTrack
	isScreen              *atomic.Bool
	onTrackEndedCallbacks []func()
}

func (t *clientTrack) ID() string {
	return t.id
}

func (t *clientTrack) Client() *Client {
	return t.client
}

func (t *clientTrack) Kind() webrtc.RTPCodecType {
	return t.remoteTrack.track.Kind()
}

func (t *clientTrack) push(rtp rtp.Packet, quality QualityLevel) {
	if t.client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	if t.Kind() == webrtc.RTPCodecTypeAudio {
		// do something here with audio level
	}

	if err := t.localTrack.WriteRTP(&rtp); err != nil {
		glog.Error("clienttrack: error on write rtp", err)
	}
}

func (t *clientTrack) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *clientTrack) IsScreen() bool {
	return t.isScreen.Load()
}

func (t *clientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *clientTrack) OnTrackEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, callback)
}

func (t *clientTrack) onTrackEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.onTrackEndedCallbacks {
		callback()
	}
}

func (t *clientTrack) IsSimulcast() bool {
	return false
}

func (t *clientTrack) IsScaleable() bool {
	return false
}

func (t *clientTrack) RequestPLI() {
	if err := t.remoteTrack.sendPLI(); err != nil {
		glog.Error("clienttrack: error on send pli", err)
	}
}

func (t *clientTrack) SetMaxQuality(_ QualityLevel) {
	// do nothing
}

func (t *clientTrack) MaxQuality() QualityLevel {
	return QualityHigh
}
