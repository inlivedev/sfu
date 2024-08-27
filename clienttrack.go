package sfu

import (
	"context"
	"sync"

	"github.com/inlivedev/sfu/pkg/packetmap"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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
	OnEnded(func())
}

type clientTrack struct {
	id                    string
	streamid              string
	context               context.Context
	mu                    sync.RWMutex
	client                *Client
	kind                  webrtc.RTPCodecType
	mimeType              string
	localTrack            *webrtc.TrackLocalStaticRTP
	remoteTrack           *remoteTrack
	baseTrack             *baseTrack
	packetmap             *packetmap.Map
	isScreen              bool
	ssrc                  webrtc.SSRC
	onTrackEndedCallbacks []func()
}

func newClientTrack(c *Client, t ITrack, isScreen bool, localTrack *webrtc.TrackLocalStaticRTP) *clientTrack {
	ctx, cancel := context.WithCancel(t.Context())
	track := t.(*Track)

	if localTrack == nil {
		localTrack = track.createLocalTrack()
	}

	ct := &clientTrack{
		id:                    localTrack.ID(),
		streamid:              localTrack.StreamID(),
		context:               ctx,
		mu:                    sync.RWMutex{},
		client:                c,
		kind:                  localTrack.Kind(),
		mimeType:              localTrack.Codec().MimeType,
		localTrack:            localTrack,
		remoteTrack:           track.remoteTrack,
		baseTrack:             track.base,
		isScreen:              isScreen,
		ssrc:                  track.remoteTrack.track.SSRC(),
		onTrackEndedCallbacks: make([]func(), 0),
		packetmap:             &packetmap.Map{},
	}

	t.OnEnded(func() {
		ct.onEnded()
		cancel()
	})

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
		t.client.log.Errorf("clienttrack: error on get sender bitrate %s", err.Error())
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

	ok, newseqno, _ := t.packetmap.Map(p.SequenceNumber, 0)
	if !ok {
		return
	}

	p.SequenceNumber = newseqno

	if t.Kind() == webrtc.RTPCodecTypeAudio {
		// do something here with audio level
	}

	// if video quality is none we need to send blank frame
	// make sure the player is paused when the quality is none.
	// quality none only possible when the video is not displayed
	if t.Kind() == webrtc.RTPCodecTypeVideo {
		quality := t.getQuality()
		if quality == QualityNone {
			if ok := t.packetmap.Drop(p.SequenceNumber, 0); ok {
				return
			}

			p.Payload = p.Payload[:0]
		}
	}

	if err := t.localTrack.WriteRTP(p); err != nil {
		t.client.log.Errorf("clienttrack: error on write rtp", err)
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
	if t.Kind() == webrtc.RTPCodecTypeVideo {
		return QualityHigh
	}

	return QualityAudio
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

func (t *clientTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, f)
}

func (t *clientTrack) onEnded() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, f := range t.onTrackEndedCallbacks {
		f()
	}
}

func (t *clientTrack) getQuality() QualityLevel {
	claim := t.client.bitrateController.GetClaim(t.ID())

	if claim == nil {
		t.client.log.Warnf("scalabletrack: claim is nil")
		return QualityNone
	}

	return min(t.MaxQuality(), claim.Quality(), Uint32ToQualityLevel(t.client.quality.Load()))
}

func qualityLevelToPreset(lvl QualityLevel) (qualityPreset QualityPreset) {

	return DefaultQualityPresets[lvl]
}
