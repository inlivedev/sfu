package sfu

import (
	"sync"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
)

type IQualityRef interface {
	GetSID() uint8
	GetTID() uint8
}

type QualityHighRef struct {
	SID uint8
	TID uint8
}

func (q QualityHighRef) GetSID() uint8 {
	return q.SID
}

func (q QualityHighRef) GetTID() uint8 {
	return q.TID
}

type QualityMidRef struct {
	SID uint8
	TID uint8
}

func (q QualityMidRef) GetSID() uint8 {
	return q.SID
}

func (q QualityMidRef) GetTID() uint8 {
	return q.TID
}

type QualityLowRef struct {
	SID uint8
	TID uint8
}

func (q QualityLowRef) GetSID() uint8 {
	return q.SID
}

func (q QualityLowRef) GetTID() uint8 {
	return q.TID
}

type QualityRef struct {
	High QualityHighRef
	Mid  QualityMidRef
	Low  QualityLowRef
}

func DefaultQualityRef() QualityRef {
	return QualityRef{
		High: QualityHighRef{
			SID: 2,
			TID: 2,
		},
		Mid: QualityMidRef{
			SID: 1,
			TID: 1,
		},
		Low: QualityLowRef{
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
	pid                   uint16
	tid                   uint8
	sid                   uint8
	lastTimestamp         uint32
	isScreen              bool
	isEnded               bool
	onTrackEndedCallbacks []func()
	dropCounter           uint16
	qualityRef            QualityRef
}

func (t *scaleabletClientTrack) Client() *Client {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.client
}

func (t *scaleabletClientTrack) isFirstKeyframePacket(p *rtp.Packet) bool {
	isKeyframe := IsKeyframe(t.mimeType, p)

	return isKeyframe && t.lastTimestamp != p.Timestamp
}

func (t *scaleabletClientTrack) writeRTP(p *rtp.Packet) {
	t.lastTimestamp = p.Timestamp
	t.sequenceNumber = p.SequenceNumber

	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("track: error on write rtp", err)
	}
}

// this where the temporal and spatial layers are will be decided to be sent to the client or not
// compare it with the claimed quality to decide if the packet should be sent or not
func (t *scaleabletClientTrack) push(p *rtp.Packet, _ QualityLevel) {
	var qualityRef IQualityRef

	vp9Packet := &codecs.VP9Packet{}
	if _, err := vp9Packet.Unmarshal(p.Payload); err != nil {
		glog.Error("scalabletrack: error on unmarshal vp9 packet", err)
		t.dropCounter++
		return
	}

	if t.client.isDebug {
		// glog.Info("scalabletrack: received packet ", p.SequenceNumber, " PID: ", vp9Packet.PictureID, " SID: ", vp9Packet.SID, " TID: ", vp9Packet.TID)
	}

	if t.spatsialCount == 0 || t.temporalCount == 0 {
		t.temporalCount = vp9Packet.NG + 1
		t.spatsialCount = vp9Packet.NS + 1
	}

	adjustment, quality := t.getAdjustment()

	if quality == QualityNone {
		t.dropCounter++
		return
	}

	switch quality {
	case QualityHigh:
		qualityRef = t.qualityRef.High
	case QualityMid:
		qualityRef = t.qualityRef.Mid
	case QualityLow:
		qualityRef = t.qualityRef.Low
	}

	switch adjustment {
	case decreaseBitrate:
		// down-switching can be done at the start of frame
		if vp9Packet.B && vp9Packet.SID == qualityRef.GetSID() {
			if t.sid > qualityRef.GetSID() {
				t.sid = qualityRef.GetSID()
			}

			if t.tid > qualityRef.GetTID() {
				t.tid = qualityRef.GetTID()
			}
		}

		if t.tid == qualityRef.GetTID() && t.sid == qualityRef.GetSID() {
			t.lastQuality = QualityLevel(quality)
		}

	case increaseBitrate:
		if vp9Packet.B {
			if !vp9Packet.P && vp9Packet.SID == qualityRef.GetSID() {
				// up-switching to a current spatial layer's frame is possible from directly lower
				// spatial layer frame.
				t.sid = qualityRef.GetSID()
			}

			if vp9Packet.U && vp9Packet.TID == qualityRef.GetTID() {
				// up-switching to a higher temporal layer's frame is possible from a directly lower
				// temporal layer frame.
				t.tid = qualityRef.GetTID()
			}

			if t.tid == qualityRef.GetTID() && t.sid == qualityRef.GetSID() {
				t.lastQuality = QualityLevel(quality)
			}
		}
	}

	// if vp9Packet.Z && vp9Packet.SID < t.sid {
	// 	// This enables a decoder which is
	// 	// targeting a higher spatial layer to know that it can safely
	// 	// discard this packet's frame without processing it, without having
	// 	// to wait for the "D" bit in the higher-layer frame

	// 	glog.Info("scalebaletrack: current TID: ", t.tid, " current SID: ", t.sid, " lastQuality: ", t.lastQuality)
	// 	glog.Info("scalabletrack: dropping packet ", p.SequenceNumber, " PID: ", vp9Packet.PictureID, " SID: ", vp9Packet.SID, " TID: ", vp9Packet.TID)

	// 	// drop the packet
	// 	t.dropCounter++
	// 	return
	// }

	if t.tid >= vp9Packet.TID && t.sid >= vp9Packet.SID {
		p.SequenceNumber = p.SequenceNumber - t.dropCounter
		if t.client.isDebug {
			glog.Info("tid: ", t.tid, " sid: ", t.sid)
			glog.Info("scalabletrack: writing packet ", p.SequenceNumber, " PID: ", vp9Packet.PictureID, " SID: ", vp9Packet.SID, " TID: ", vp9Packet.TID)
		}
		t.writeRTP(p)
	} else {
		t.dropCounter++
	}

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

func (t *scaleabletClientTrack) getAdjustment() (bitrateAdjustment, QualityLevel) {
	lastQuality := t.LastQuality()
	claim := t.client.bitrateController.GetClaim(t.ID())

	if claim == nil {
		glog.Warning("scalabletrack: claim is nil")
		return keepBitrate, QualityNone
	}

	quality := min(t.maxQuality, claim.quality, Uint32ToQualityLevel(t.client.quality.Load()))
	if quality < lastQuality {
		return decreaseBitrate, quality
	} else if quality > lastQuality {
		return increaseBitrate, quality
	}

	return keepBitrate, quality
}
