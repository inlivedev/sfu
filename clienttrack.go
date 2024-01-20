package sfu

import (
	"context"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type iClientTrack interface {
	push(rtp rtp.Packet, quality QualityLevel)
	ID() string
	Context() context.Context
	Kind() webrtc.RTPCodecType
	LocalTrack() *webrtc.TrackLocalStaticRTP
	IsScreen() bool
	IsSimulcast() bool
	IsScaleable() bool
	SetSourceType(TrackType)
	Client() *Client
	RequestPLI()
	SetMaxQuality(quality QualityLevel)
	MaxQuality() QualityLevel
}

type clientTrack struct {
	id          string
	context     context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	client      *Client
	kind        webrtc.RTPCodecType
	mimeType    string
	localTrack  *webrtc.TrackLocalStaticRTP
	remoteTrack *remoteTrack
	isScreen    bool
	packetChan  chan rtp.Packet
}

func newClientTrack(c *Client, t *Track, isScreen bool) *clientTrack {
	ctx, cancel := context.WithCancel(t.Context())
	ct := &clientTrack{
		id:          t.base.id,
		context:     ctx,
		cancel:      cancel,
		mu:          sync.RWMutex{},
		client:      c,
		kind:        t.base.kind,
		mimeType:    t.remoteTrack.track.Codec().MimeType,
		localTrack:  t.createLocalTrack(),
		remoteTrack: t.remoteTrack,
		isScreen:    isScreen,
		packetChan:  make(chan rtp.Packet, 1024),
	}

	ct.startWorker()

	return ct
}

func (t *clientTrack) startWorker() {
	go func() {
		for {
			select {
			case <-t.context.Done():
				close(t.packetChan)
				return
			case p := <-t.packetChan:
				t.processPacket(p)
			}
		}
	}()
}

func (t *clientTrack) push(p rtp.Packet, _ QualityLevel) {
	if t.client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	t.packetChan <- p
}

func (t *clientTrack) ID() string {
	return t.id
}

func (t *clientTrack) Context() context.Context {
	return t.context
}

func (t *clientTrack) Client() *Client {
	return t.client
}

func (t *clientTrack) Kind() webrtc.RTPCodecType {
	return t.remoteTrack.track.Kind()
}

func (t *clientTrack) processPacket(rtp rtp.Packet) {
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
	return t.isScreen
}

func (t *clientTrack) SetSourceType(sourceType TrackType) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.isScreen = sourceType == TrackTypeScreen
}

func (t *clientTrack) IsSimulcast() bool {
	return false
}

func (t *clientTrack) IsScaleable() bool {
	return false
}

func (t *clientTrack) RequestPLI() {
	t.remoteTrack.sendPLI()
}

func (t *clientTrack) SetMaxQuality(_ QualityLevel) {
	// do nothing
}

func (t *clientTrack) MaxQuality() QualityLevel {
	return QualityHigh
}
