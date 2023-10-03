package sfu

import (
	"image"
	"image/color"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

type iClientTrack interface {
	getCurrentBitrate() uint32
	push(rtp *rtp.Packet, quality QualityLevel)
	ID() string
	Kind() webrtc.RTPCodecType
	LocalTrack() *webrtc.TrackLocalStaticRTP
	IsScreen() bool
	IsSimulcast() bool
	SetSourceType(TrackType)
	OnTrackEnded(func())
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

func (t *clientTrack) Kind() webrtc.RTPCodecType {
	return t.remoteTrack.track.Kind()
}

func (t *clientTrack) push(rtp *rtp.Packet, quality QualityLevel) {
	if t.client.peerConnection.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	if t.Kind() == webrtc.RTPCodecTypeAudio {
		// do something here with audio level
	}

	if err := t.localTrack.WriteRTP(rtp); err != nil {
		glog.Error("clienttrack: error on write rtp", err)
	}
}

func (t *clientTrack) getAudioLevel(p *rtp.Packet) rtp.AudioLevelExtension {
	audioLevel := rtp.AudioLevelExtension{}
	headerID := t.remoteTrack.getAudioLevelExtensionID()
	if headerID != 0 {
		ext := p.Header.GetExtension(headerID)
		if err := audioLevel.Unmarshal(ext); err != nil {
			glog.Error("clienttrack: error on unmarshal audio level", err)
		}
	}

	return audioLevel

}

func (t *clientTrack) getCurrentBitrate() uint32 {
	return t.remoteTrack.GetCurrentBitrate()
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

type SimulcastClientTrack struct {
	id                      string
	mu                      sync.RWMutex
	client                  *Client
	kind                    webrtc.RTPCodecType
	mimeType                string
	localTrack              *webrtc.TrackLocalStaticRTP
	remoteTrack             *simulcastTrack
	lastBlankSequenceNumber *atomic.Uint32
	sequenceNumber          *atomic.Uint32
	lastQuality             *atomic.Uint32
	lastTimestamp           *atomic.Uint32
	lastCheckQualityTS      *atomic.Int64
	isScreen                *atomic.Bool
	isEnded                 *atomic.Bool
	onTrackEndedCallbacks   []func()
}

func (t *SimulcastClientTrack) isFirstKeyframePacket(p *rtp.Packet) bool {
	isKeyframe := IsKeyframe(t.mimeType, p)

	return isKeyframe && t.lastTimestamp.Load() != p.Timestamp
}

func (t *SimulcastClientTrack) send(p *rtp.Packet, quality QualityLevel, lastQuality QualityLevel) {
	// set the last processed packet timestamp to identify if is begining of the new frame
	t.lastTimestamp.Store(p.Timestamp)
	// make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track
	sequenceDelta := uint16(0)
	// credit to https://github.com/k0nserv for helping me with this on Pion Slack channel
	switch quality {
	case QualityHigh:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackHighBaseTS) - t.remoteTrack.remoteTrackHighBaseTS)
		sequenceDelta = t.remoteTrack.highSequence - t.remoteTrack.lastHighSequence
	case QualityMid:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackMidBaseTS) - t.remoteTrack.remoteTrackMidBaseTS)
		sequenceDelta = t.remoteTrack.midSequence - t.remoteTrack.lastMidSequence
	case QualityLow:
		p.Timestamp = t.remoteTrack.baseTS + ((p.Timestamp - t.remoteTrack.remoteTrackLowBaseTS) - t.remoteTrack.remoteTrackLowBaseTS)
		sequenceDelta = t.remoteTrack.lowSequence - t.remoteTrack.lastLowSequence
	}

	t.sequenceNumber.Add(uint32(sequenceDelta))
	p.SequenceNumber = uint16(t.sequenceNumber.Load())

	if lastQuality != quality {
		t.lastQuality.Store(uint32(quality))
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("track: error on write rtp", err)
	}
}

// Currently not used, plan to use for other codec than h264
func (t *SimulcastClientTrack) sendBlankFrame(packetRef *rtp.Packet) {
	timestamp := t.remoteTrack.baseTS + ((packetRef.Timestamp - t.remoteTrack.remoteTrackLowBaseTS) - t.remoteTrack.remoteTrackLowBaseTS)
	t.lastBlankSequenceNumber.Add(1)
	sequenceNumber := t.lastBlankSequenceNumber.Load()

	img := image.NewGray(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetGray(x, y, color.Gray{Y: 0}) // Set pixel to black
		}
	}

	sample := &media.Sample{
		Data:            img.Pix,
		Timestamp:       time.Now(),
		Duration:        time.Second,
		PacketTimestamp: timestamp,
	}

	lastSequence, err := SendBlackImageFrames(uint16(sequenceNumber), t.localTrack, sample)
	if err != nil {
		glog.Error("track: error on write black frame", err)
	}

	t.sequenceNumber.Store(uint32(lastSequence))
	t.lastBlankSequenceNumber.Store(uint32(lastSequence))
}

func (t *SimulcastClientTrack) push(p *rtp.Packet, quality QualityLevel) {

	var trackQuality QualityLevel

	lastQuality := t.LastQuality()

	isFirstKeyframePacket := t.isFirstKeyframePacket(p)

	// check if it's a first packet to send
	if lastQuality == QualityNone && t.sequenceNumber.Load() == 0 {
		// we try to send the low quality first	if the track is active and fallback to upper quality if not
		if t.remoteTrack.isTrackActive(QualityLow) {
			trackQuality = QualityLow
			t.lastQuality.Store(uint32(QualityLow))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI(QualityLow)
		} else if t.remoteTrack.isTrackActive(QualityMid) {
			trackQuality = QualityMid
			t.lastQuality.Store(uint32(QualityMid))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI(QualityMid)
		} else if t.remoteTrack.isTrackActive(QualityHigh) {
			trackQuality = QualityHigh
			t.lastQuality.Store(uint32(QualityHigh))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI(QualityHigh)
		} else {
			trackQuality = QualityNone
		}

		t.remoteTrack.onRemoteTrackAdded(func(remote *remoteTrack) {
			quality := RIDToQuality(remote.track.RID())
			t.remoteTrack.sendPLI(quality)
		})
	} else {
		trackQuality = lastQuality

		// lastCheckQualityDuration := time.Since(time.Unix(0, t.lastCheckQualityTS.Load()))

		if isFirstKeyframePacket { // && lastCheckQualityDuration.Seconds() >= 1 {
			trackQuality = t.client.bitrateController.GetQuality(t)

			// update latest keyframe timestamp
			// TODO: currently not use anywhere but useful to detect if the track is active or need to refresh full picture
			switch quality {
			case QualityHigh:
				t.remoteTrack.lastHighKeyframeTS.Store(time.Now().UnixNano())
			case QualityMid:
				t.remoteTrack.lastMidKeyframeTS.Store(time.Now().UnixNano())
			case QualityLow:
				t.remoteTrack.lastLowKeyframeTS.Store(time.Now().UnixNano())
			}

			if trackQuality == QualityNone {
				// could be because not enough bandwidth to send any track
				t.lastQuality.Store(uint32(trackQuality))
			}
		}
	}

	if trackQuality == quality {
		t.send(p, trackQuality, lastQuality)
	} else if trackQuality == QualityNone && quality == QualityLow {
		if isFirstKeyframePacket {
			glog.Warning("clienttrack: no quality level to send")
		}

		if t.localTrack.Codec().MimeType == webrtc.MimeTypeH264 && isFirstKeyframePacket {
			// if codec is h264, send a blank frame once
			p.Payload = getH264BlankFrame()
			t.send(p, QualityLow, lastQuality)
		} else if t.localTrack.Codec().MimeType != webrtc.MimeTypeH264 && t.remoteTrack.isTrackActive(QualityLow) {
			// if codec is not h264, send a low quality packet
			t.send(p, QualityLow, lastQuality)
		} else {
			// last effort, send the last quality
			t.send(p, lastQuality, lastQuality)
		}
	}
}

func (t *SimulcastClientTrack) GetRemoteTrack() *remoteTrack {
	lastQuality := Uint32ToQualityLevel(t.lastQuality.Load())
	// lastQuality := t.lastQuality
	switch lastQuality {
	case QualityHigh:
		return t.remoteTrack.remoteTrackHigh
	case QualityMid:
		return t.remoteTrack.remoteTrackMid
	case QualityLow:
		return t.remoteTrack.remoteTrackLow
	default:
		if t.remoteTrack.isTrackActive(QualityHigh) {
			return t.remoteTrack.remoteTrackHigh
		}

		if t.remoteTrack.isTrackActive(QualityMid) {
			return t.remoteTrack.remoteTrackMid
		}

		if t.remoteTrack.isTrackActive(QualityLow) {
			return t.remoteTrack.remoteTrackLow
		}
	}

	return nil
}

func (t *SimulcastClientTrack) getCurrentBitrate() uint32 {
	currentTrack := t.GetRemoteTrack()
	if currentTrack == nil {
		return 0
	}

	return currentTrack.GetCurrentBitrate()
}

func (t *SimulcastClientTrack) ID() string {
	return t.id
}

func (t *SimulcastClientTrack) Kind() webrtc.RTPCodecType {
	return t.kind
}

func (t *SimulcastClientTrack) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *SimulcastClientTrack) IsScreen() bool {
	return t.isScreen.Load()
}

func (t *SimulcastClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *SimulcastClientTrack) LastQuality() QualityLevel {
	return Uint32ToQualityLevel(t.lastQuality.Load())
}

func (t *SimulcastClientTrack) OnTrackEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, callback)
}

func (t *SimulcastClientTrack) onTrackEnded() {
	if t.isEnded.Load() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.onTrackEndedCallbacks {
		callback()
	}

	t.isEnded.Store(true)
}

func (t *SimulcastClientTrack) IsSimulcast() bool {
	return true
}

type clientTrackList struct {
	mu     sync.RWMutex
	tracks []iClientTrack
}

func (l *clientTrackList) Add(track iClientTrack) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	l.tracks = append(l.tracks, track)
}

func (l *clientTrackList) Remove(id string) {
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

	var track iClientTrack

	for _, t := range l.tracks {
		if t.ID() == id {
			track = t
			break
		}
	}

	return track
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
