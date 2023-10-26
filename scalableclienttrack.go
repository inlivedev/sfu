package sfu

import (
	"sync"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
)

type IQualityPreset interface {
	GetSID() uint8
	GetTID() uint8
}

type QualityHighPreset struct {
	SID uint8
	TID uint8
}

func (q QualityHighPreset) GetSID() uint8 {
	return q.SID
}

func (q QualityHighPreset) GetTID() uint8 {
	return q.TID
}

type QualityMidPreset struct {
	SID uint8
	TID uint8
}

func (q QualityMidPreset) GetSID() uint8 {
	return q.SID
}

func (q QualityMidPreset) GetTID() uint8 {
	return q.TID
}

type QualityLowPreset struct {
	SID uint8
	TID uint8
}

func (q QualityLowPreset) GetSID() uint8 {
	return q.SID
}

func (q QualityLowPreset) GetTID() uint8 {
	return q.TID
}

type QualityPreset struct {
	High QualityHighPreset
	Mid  QualityMidPreset
	Low  QualityLowPreset
}

func DefaultQualityPreset() QualityPreset {
	return QualityPreset{
		High: QualityHighPreset{
			SID: 2,
			TID: 2,
		},
		Mid: QualityMidPreset{
			SID: 1,
			TID: 1,
		},
		Low: QualityLowPreset{
			SID: 0,
			TID: 0,
		},
	}
}

type scaleabletClientTrack struct {
	id                    string
	mu                    sync.RWMutex
	client                *Client
	kind                  webrtc.RTPCodecType
	mimeType              string
	localTrack            *webrtc.TrackLocalStaticRTP
	remoteTrack           *track
	sequenceNumber        uint16
	lastQuality           QualityLevel
	maxQuality            QualityLevel
	temporalCount         uint8
	spatsialCount         uint8
	tid                   uint8
	sid                   uint8
	lastTimestamp         uint32
	isScreen              bool
	isEnded               bool
	onTrackEndedCallbacks []func()
	dropCounter           uint16
	qualityPreset         QualityPreset
}

func (t *scaleabletClientTrack) Client() *Client {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.client
}

func (t *scaleabletClientTrack) writeRTP(p *rtp.Packet) {
	t.lastTimestamp = p.Timestamp
	t.sequenceNumber = p.SequenceNumber

	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("track: error on write rtp", err)
	}
}

func (t *scaleabletClientTrack) isKeyframe(vp9 *codecs.VP9Packet) bool {
	if len(vp9.Payload) < 1 {
		return false
	}
	if !vp9.B {
		return false
	}

	if (vp9.Payload[0] & 0xc0) != 0x80 {
		return false
	}

	profile := (vp9.Payload[0] >> 4) & 0x3
	if profile != 3 {
		return (vp9.Payload[0]&0xC) == 0 && true
	}
	return (vp9.Payload[0]&0x6) == 0 && true
}

// this where the temporal and spatial layers are will be decided to be sent to the client or not
// compare it with the claimed quality to decide if the packet should be sent or not
func (t *scaleabletClientTrack) push(p *rtp.Packet, _ QualityLevel) {
	var qualityPreset IQualityPreset

	vp9Packet := &codecs.VP9Packet{}
	if _, err := vp9Packet.Unmarshal(p.Payload); err != nil {
		glog.Error("scalabletrack: error on unmarshal vp9 packet", err)
		t.send(p)
		return
	}

	if t.spatsialCount == 0 || t.temporalCount == 0 {
		t.temporalCount = vp9Packet.NG + 1
		t.spatsialCount = vp9Packet.NS + 1
	}

	quality := t.getQuality()

	if quality == QualityNone {
		t.dropCounter++
		return
	}

	switch quality {
	case QualityHigh:
		qualityPreset = t.qualityPreset.High
	case QualityMid:
		qualityPreset = t.qualityPreset.Mid
	case QualityLow:
		qualityPreset = t.qualityPreset.Low
	}

	isKeyframe := t.isKeyframe(vp9Packet)

	if vp9Packet.B && t.sid != qualityPreset.GetSID() {
		if vp9Packet.SID == qualityPreset.GetSID() && !vp9Packet.P {
			t.sid = qualityPreset.GetSID()
		} else {
			t.RequestPLI()
		}
	}

	if vp9Packet.B && t.tid != qualityPreset.GetTID() {
		if isKeyframe || t.tid > qualityPreset.GetTID() || vp9Packet.U {
			t.tid = qualityPreset.GetTID()
		}
	}

	if t.tid == qualityPreset.GetTID() && t.sid == qualityPreset.GetSID() {
		t.lastQuality = quality
	}

	if vp9Packet.E && t.sid == vp9Packet.SID {
		p.Marker = true
	}

	if vp9Packet.TID == 0 && vp9Packet.SID == 0 {
		t.send(p)
		return
	}

	// Can we drop the packet
	// vp9Packet.Z && vp9Packet.SID < t.sid
	// This enables a decoder which is
	// targeting a higher spatial layer to know that it can safely
	// discard this packet's frame without processing it, without having
	// to wait for the "D" bit in the higher-layer frame
	if t.tid < vp9Packet.TID || t.sid < vp9Packet.SID || (t.sid > vp9Packet.SID && vp9Packet.Z) {
		t.dropCounter++
		return
	}

	t.send(p)
}

func (t *scaleabletClientTrack) send(p *rtp.Packet) {
	p.SequenceNumber = p.SequenceNumber - t.dropCounter
	t.writeRTP(p)
}

func (t *scaleabletClientTrack) RemoteTrack() *remoteTrack {
	return t.remoteTrack.remoteTrack
}

func (t *scaleabletClientTrack) getCurrentBitrate() uint32 {
	currentTrack := t.RemoteTrack()
	if currentTrack == nil {
		return 0
	}

	return currentTrack.GetCurrentBitrate()
}

func (t *scaleabletClientTrack) ID() string {
	return t.id
}

func (t *scaleabletClientTrack) Kind() webrtc.RTPCodecType {
	return t.kind
}

func (t *scaleabletClientTrack) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *scaleabletClientTrack) IsScreen() bool {
	return t.isScreen
}

func (t *scaleabletClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen = (sourceType == TrackTypeScreen)
}

func (t *scaleabletClientTrack) LastQuality() QualityLevel {
	return QualityLevel(t.lastQuality)
}

func (t *scaleabletClientTrack) OnTrackEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, callback)
}

func (t *scaleabletClientTrack) onTrackEnded() {
	if t.isEnded {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.onTrackEndedCallbacks {
		callback()
	}

	t.isEnded = true
}

func (t *scaleabletClientTrack) SetMaxQuality(quality QualityLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.maxQuality = quality
}

func (t *scaleabletClientTrack) MaxQuality() QualityLevel {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.maxQuality
}

func (t *scaleabletClientTrack) IsSimulcast() bool {
	return false
}

func (t *scaleabletClientTrack) IsScaleable() bool {
	return true
}

func (t *scaleabletClientTrack) RequestPLI() {
	t.remoteTrack.remoteTrack.sendPLI()
}

func (t *scaleabletClientTrack) getQuality() QualityLevel {
	lastQuality := t.LastQuality()
	claim := t.client.bitrateController.GetClaim(t.ID())

	if claim == nil {
		glog.Warning("scalabletrack: claim is nil")
		return QualityNone
	}

	quality := min(t.maxQuality, claim.quality, Uint32ToQualityLevel(t.client.quality.Load()))
	if quality < lastQuality {
		return lastQuality - 1
	} else if quality > lastQuality {
		return lastQuality + 1
	}

	return lastQuality
}
