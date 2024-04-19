package sfu

import (
	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/packetmap"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

type IQualityPreset interface {
	GetSID() uint8
	GetTID() uint8
}

type QualityPreset struct {
	SID uint8 `json:"sid" example:"2"`
	TID uint8 `json:"tid" example:"2"`
}

func (q QualityPreset) GetSID() uint8 {
	return q.SID
}

func (q QualityPreset) GetTID() uint8 {
	return q.TID
}

type QualityPresets struct {
	High QualityPreset `json:"high"`
	Mid  QualityPreset `json:"mid"`
	Low  QualityPreset `json:"low"`
}

func DefaultQualityPresets() QualityPresets {
	return QualityPresets{
		High: QualityPreset{
			SID: 2,
			TID: 2,
		},
		Mid: QualityPreset{
			SID: 1,
			TID: 1,
		},
		Low: QualityPreset{
			SID: 0,
			TID: 0,
		},
	}
}

type scaleableClientTrack struct {
	*clientTrack
	lastQuality    QualityLevel
	maxQuality     QualityLevel
	tid            uint8
	sid            uint8
	lastTimestamp  uint32
	lastSequence   uint16
	qualityPresets QualityPresets
	init           bool
	packetmap      *packetmap.Map
}

func newScaleableClientTrack(
	c *Client,
	t *Track,
	qualityPresets QualityPresets,
) *scaleableClientTrack {

	sct := &scaleableClientTrack{
		clientTrack:    newClientTrack(c, t, false, nil),
		qualityPresets: qualityPresets,
		maxQuality:     QualityHigh,
		lastQuality:    QualityHigh,
		tid:            qualityPresets.High.TID,
		sid:            qualityPresets.High.SID,
		packetmap:      &packetmap.Map{},
	}

	return sct
}

func (t *scaleableClientTrack) isKeyframe(vp9 *codecs.VP9Packet) bool {
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

func (t *scaleableClientTrack) push(p *rtp.Packet, _ QualityLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var qualityPreset IQualityPreset

	var isLatePacket bool

	vp9Packet := &codecs.VP9Packet{}
	if _, err := vp9Packet.Unmarshal(p.Payload); err != nil {
		_ = t.packetmap.Drop(p.SequenceNumber, vp9Packet.PictureID)

		return
	}

	quality := t.getQuality()
	if quality == QualityNone {
		_ = t.packetmap.Drop(p.SequenceNumber, vp9Packet.PictureID)

		glog.Info("scalabletrack: packet ", p.SequenceNumber, " is dropped because of quality none")
		return
	}

	switch quality {
	case QualityHigh:
		qualityPreset = t.qualityPresets.High
	case QualityMid:
		qualityPreset = t.qualityPresets.Mid
	case QualityLow:
		qualityPreset = t.qualityPresets.Low
	}

	targetSID := qualityPreset.GetSID()
	targetTID := qualityPreset.GetTID()

	if !t.init {
		t.init = true
		t.sid = targetSID
		t.tid = targetTID
	} else {
		isLatePacket = IsRTPPacketLate(p.SequenceNumber, t.lastSequence)
	}

	t.lastSequence = p.Header.SequenceNumber

	currentTID := t.tid
	currentSID := t.sid

	// check if possible to scale up/down temporal layer
	if t.tid < targetTID && !isLatePacket {
		if vp9Packet.U && vp9Packet.B && currentTID < vp9Packet.TID && vp9Packet.TID <= targetTID {
			// scale temporal up
			t.tid = vp9Packet.TID
			currentTID = t.tid
		}
	} else if t.tid > targetTID && !isLatePacket {
		if vp9Packet.E {
			// scale temporal down
			t.tid = vp9Packet.TID
		}
	}

	// check if possible to scale up spatial layer

	if currentSID < targetSID && !isLatePacket {
		if !vp9Packet.P && vp9Packet.B && currentSID < vp9Packet.SID && vp9Packet.SID <= targetSID {
			// scale spatial up
			t.sid = vp9Packet.SID
			currentSID = t.sid
		}
	} else if currentSID > targetSID && !isLatePacket {
		if vp9Packet.E {
			// scale spatsial down
			t.sid = vp9Packet.SID
		}
	}

	if vp9Packet.E && t.tid == targetTID && t.sid == targetSID {
		t.setLastQuality(quality)
	}

	if currentTID < vp9Packet.TID || currentSID < vp9Packet.SID {
		// glog.Info("scalabletrack: packet ", p.SequenceNumber, " is dropped because of currentTID ", currentTID, "  < vp9Packet.TID", vp9Packet.TID)
		ok := t.packetmap.Drop(p.SequenceNumber, vp9Packet.PictureID)
		if ok {
			return
		}
		glog.Info("scalabletrack: packet ", p.SequenceNumber, " cannot be dropped")

	}

	// mark packet as a last spatial layer packet
	if vp9Packet.E && currentSID == vp9Packet.SID && targetSID <= currentSID {
		p.Marker = true
	}

	ok, newseqno, _ := t.packetmap.Map(p.SequenceNumber, vp9Packet.PictureID)
	if !ok {
		return
	}

	p.SequenceNumber = newseqno

	t.send(p)
}

func (t *scaleableClientTrack) send(p *rtp.Packet) {
	t.lastTimestamp = p.Timestamp

	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("scaleabletrack: error on write rtp", err)
	}

}

func (t *scaleableClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen = (sourceType == TrackTypeScreen)
}

func (t *scaleableClientTrack) setLastQuality(quality QualityLevel) {
	t.lastQuality = quality
}

func (t *scaleableClientTrack) LastQuality() QualityLevel {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return QualityLevel(t.lastQuality)
}

func (t *scaleableClientTrack) SetMaxQuality(quality QualityLevel) {
	t.mu.Lock()
	t.maxQuality = quality
	t.mu.Unlock()

	t.RequestPLI()
}

func (t *scaleableClientTrack) MaxQuality() QualityLevel {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.maxQuality
}

func (t *scaleableClientTrack) IsSimulcast() bool {
	return false
}

func (t *scaleableClientTrack) IsScaleable() bool {
	return true
}

func (t *scaleableClientTrack) RequestPLI() {
	go t.remoteTrack.sendPLI()
}

func (t *scaleableClientTrack) getQuality() QualityLevel {
	claim := t.client.bitrateController.GetClaim(t.ID())

	if claim == nil {
		glog.Warning("scalabletrack: claim is nil")
		return QualityNone
	}

	return min(t.maxQuality, claim.Quality(), Uint32ToQualityLevel(t.client.quality.Load()))
}
