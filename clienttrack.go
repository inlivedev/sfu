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
	StreamID() string
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
	streamid    string
	context     context.Context
	mu          sync.RWMutex
	client      *Client
	kind        webrtc.RTPCodecType
	mimeType    string
	localTrack  *webrtc.TrackLocalStaticRTP
	remoteTrack *remoteTrack
	baseTrack   *baseTrack
	isScreen    bool
	ssrc        webrtc.SSRC
}

func newClientTrack(c *Client, t *Track, isScreen bool, localTrack *webrtc.TrackLocalStaticRTP) *clientTrack {
	ctx, cancel := context.WithCancel(t.Context())

	if localTrack == nil {
		localTrack = t.createLocalTrack()
	}

	ct := &clientTrack{
		id:          localTrack.ID(),
		streamid:    localTrack.StreamID(),
		context:     ctx,
		mu:          sync.RWMutex{},
		client:      c,
		kind:        localTrack.Kind(),
		mimeType:    localTrack.Codec().MimeType,
		localTrack:  localTrack,
		remoteTrack: t.remoteTrack,
		baseTrack:   t.base,
		isScreen:    isScreen,
		ssrc:        t.remoteTrack.track.SSRC(),
	}

	go func() {
		defer cancel()
		<-ctx.Done()
	}()

	return ct
}

func (t *clientTrack) ID() string {
	return t.id
}

func (t *clientTrack) StreamID() string {
	return t.streamid
}

func (t *clientTrack) ReceiveBitrate() uint32 {
	if t.remoteTrack == nil || t.client == nil {
		return 0
	}

	bitrate, err := t.baseTrack.client.stats.GetReceiverBitrate(t.remoteTrack.track.ID(), t.remoteTrack.track.RID())
	if err != nil {
		return 0
	}

	return bitrate
}

func (t *clientTrack) SendBitrate() uint32 {
	bitrate, err := t.client.stats.GetSenderBitrate(t.ID())
	if err != nil {
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
