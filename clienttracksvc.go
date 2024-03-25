package sfu

import (
	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
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

type scaleableClientTrack struct {
	*clientTrack
	lastQuality     QualityLevel
	maxQuality      QualityLevel
	tid             uint8
	sid             uint8
	lastTimestamp   uint32
	lastSequence    uint16
	baseSequence    uint16
	qualityPreset   QualityPreset
	hasInterPicture bool
	init            bool
	packetCaches    *packetCaches
}

func newScaleableClientTrack(
	c *Client,
	t *Track,
	qualityPreset QualityPreset,
) *scaleableClientTrack {

	sct := &scaleableClientTrack{
		clientTrack:   newClientTrack(c, t, false),
		qualityPreset: qualityPreset,
		maxQuality:    QualityHigh,
		lastQuality:   QualityHigh,
		tid:           qualityPreset.High.TID,
		sid:           qualityPreset.High.SID,
		packetCaches:  newPacketCaches(),
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
	var qualityPreset IQualityPreset

	t.mu.RLock()
	isLatePacket := IsRTPPacketLate(p.SequenceNumber, t.lastSequence)
	t.mu.RUnlock()

	if !t.init {
		t.init = true
		t.baseSequence = p.SequenceNumber
	}

	vp9Packet := &codecs.VP9Packet{}
	if _, err := vp9Packet.Unmarshal(p.Payload); err != nil {

		return
	}

	quality := t.getQuality()
	if quality == QualityNone {
		t.baseSequence++
		glog.Info("scalabletrack: packet ", p.SequenceNumber, " is dropped because of quality none")
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

	targetSID := qualityPreset.GetSID()
	targetTID := qualityPreset.GetTID()

	if !t.hasInterPicture {
		if !vp9Packet.P {
			t.baseSequence++
			t.RequestPLI()
			glog.Info("scalabletrack: packet ", p.SequenceNumber, " client ", t.client.ID(), " is dropped because of no intra frame")
			return

		}

		t.hasInterPicture = true
		t.sid = targetSID
		t.tid = targetTID
	}

	currentBaseSequence := t.baseSequence
	currentTID := t.tid
	currentSID := t.sid

	var err error

	if isLatePacket {
		glog.Info("scalabletrack: packet ", p.SequenceNumber, " is late")
		currentBaseSequence, currentSID, currentTID, err = t.packetCaches.GetDecision(p.SequenceNumber, currentBaseSequence, currentSID, currentTID)
		if err != nil &&
			(!vp9Packet.P && vp9Packet.B && currentSID < vp9Packet.SID && vp9Packet.SID <= targetSID) ||
			(currentSID > targetSID && vp9Packet.E) {
			t.RequestPLI()
		}
	}

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

	if currentTID < vp9Packet.TID {
		// glog.Info("scalabletrack: packet ", p.SequenceNumber, " is dropped because of currentTID ", currentTID, "  < vp9Packet.TID", vp9Packet.TID)
		t.baseSequence++
		return
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
		t.SetLastQuality(quality)
	}

	if currentSID < vp9Packet.SID {
		t.baseSequence++
		// glog.Info("scalabletrack: packet ", p.SequenceNumber, " is dropped because of currentSID ", currentSID, " < vp9Packet.SID", vp9Packet.SID)
		return
	}

	// mark packet as a last spatial layer packet
	if vp9Packet.E && currentSID == vp9Packet.SID && targetSID <= currentSID {
		p.Marker = true
	}

	t.send(p, currentBaseSequence, vp9Packet.SID, vp9Packet.TID, t.sid, t.tid, isLatePacket)
}

func (t *scaleableClientTrack) send(p *rtp.Packet, baseSeq uint16, pSID, tSID, decidedSID, decidedTID uint8, isLate bool) {
	if !isLate {
		t.mu.Lock()
		t.lastSequence = p.Header.SequenceNumber
		t.mu.Unlock()
		t.packetCaches.Add(p.SequenceNumber, t.baseSequence, p.Timestamp, pSID, tSID, decidedSID, decidedTID)
	}

	// p.Header.SequenceNumber = p.Header.SequenceNumber - t.dropCounter

	p.Header.SequenceNumber = p.Header.SequenceNumber - baseSeq

	t.lastTimestamp = p.Timestamp

	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("scaleabletrack: error on write rtp", err)
	}

}

func (t *scaleableClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen = (sourceType == TrackTypeScreen)
}

func (t *scaleableClientTrack) SetLastQuality(quality QualityLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

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
	t.remoteTrack.sendPLI()
}

func (t *scaleableClientTrack) getQuality() QualityLevel {
	claim := t.client.bitrateController.GetClaim(t.ID())

	if claim == nil {
		glog.Warning("scalabletrack: claim is nil")
		return QualityNone
	}

	return min(t.MaxQuality(), claim.Quality(), Uint32ToQualityLevel(t.client.quality.Load()))
}
