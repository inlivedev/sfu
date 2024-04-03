package sfu

import (
	"context"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type iClientTrack interface {
	push(rtp *rtp.Packet, quality QualityLevel)
	ID() string
	Context() context.Context
	Kind() webrtc.RTPCodecType
	MimeType() string
	LocalTrack() *webrtc.TrackLocalStaticRTP
	IsScreen() bool
	IsSimulcast() bool
	IsScaleable() bool
	SetSourceType(TrackType)
	Client() *Client
	RequestPLI()
	SetMaxQuality(quality QualityLevel)
	MaxQuality() QualityLevel
	ReceiveBitrate() uint32
	SendBitrate() uint32
	Quality() QualityLevel
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
	ssrc        webrtc.SSRC
}

func newClientTrack(c *Client, t *Track, isScreen bool) *clientTrack {
	ctx, cancel := context.WithCancel(t.Context())
	ct := &clientTrack{
		id:          t.ID(),
		context:     ctx,
		cancel:      cancel,
		mu:          sync.RWMutex{},
		client:      c,
		kind:        t.base.kind,
		mimeType:    t.remoteTrack.track.Codec().MimeType,
		localTrack:  t.createLocalTrack(),
		remoteTrack: t.remoteTrack,
		isScreen:    isScreen,
		ssrc:        t.remoteTrack.track.SSRC(),
	}

	return ct
}

func (t *clientTrack) ID() string {
	return t.id
}

func (t *clientTrack) ReceiveBitrate() uint32 {
	bitrate, err := t.client.Stats().GetReceiverBitrate(t.ID())
	if err != nil {
		glog.Error("clienttrack: error on get receiver", err)
		return 0
	}

	return bitrate
}

func (t *clientTrack) SendBitrate() uint32 {
	bitrate, err := t.client.Stats().GetSenderBitrate(t.ID())
	if err != nil {
		glog.Error("clienttrack: error on get sender", err)
		return 0
	}

	return bitrate
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

func (t *clientTrack) MimeType() string {
	return t.mimeType
}

func (t *clientTrack) push(p *rtp.Packet, _ QualityLevel) {
	if t.client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	if t.Kind() == webrtc.RTPCodecTypeAudio {
		// do something here with audio level
	}

	if err := t.localTrack.WriteRTP(p); err != nil {
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

func (t *clientTrack) SSRC() webrtc.SSRC {
	return t.ssrc
}

func (t *clientTrack) Quality() QualityLevel {
	if t.Kind() == webrtc.RTPCodecTypeVideo {
		return QualityHigh
	}

	return QualityAudio
}
