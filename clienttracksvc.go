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
	dropCounter     uint16
	qualityPreset   QualityPreset
	hasInterPicture bool
	lastSequence    uint16
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

	vp9Packet := &codecs.VP9Packet{}
	if _, err := vp9Packet.Unmarshal(p.Payload); err != nil {
		t.send(p)
		return
	}

	if !t.hasInterPicture {
		if !vp9Packet.P {
			t.RequestPLI()
			glog.Info("scalabletrack: packet ", p.SequenceNumber, " client ", t.client.ID(), " is dropped because of no intra frame")
			return

		}
		t.hasInterPicture = true
	}

	quality := t.getQuality()

	if quality == QualityNone {
		t.dropCounter++
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
	currentTID := t.tid
	currentSID := t.sid

	// check if possible to scale up/down temporal layer
	if t.tid < targetTID {
		if vp9Packet.U && vp9Packet.B && currentTID < vp9Packet.TID && vp9Packet.TID <= targetTID {
			// scale temporal up
			t.tid = vp9Packet.TID
			currentTID = t.tid
		}
	} else if t.tid > targetTID {
		if vp9Packet.E {
			// scale temporal down
			t.tid = vp9Packet.TID
		}
	}

	if currentTID < vp9Packet.TID {
		// glog.Info("scalabletrack: packet ", p.SequenceNumber, " is dropped because of currentTID < vp9Packet.TID")
		t.dropCounter++
		return
	}

	// check if possible to scale up spatial layer

	if currentSID < targetSID {
		if !vp9Packet.P && vp9Packet.B && currentSID < vp9Packet.SID && vp9Packet.SID <= targetSID {
			// scale spatial up
			t.sid = vp9Packet.SID
			currentSID = t.sid
		}
	} else if currentSID > targetSID {
		if vp9Packet.E {
			// scale spatsial down
			t.sid = vp9Packet.SID
		}
	}

	if vp9Packet.E && t.tid == targetTID && t.sid == targetSID {
		t.SetLastQuality(quality)
	}

	if currentSID < vp9Packet.SID {
		t.dropCounter++
		// glog.Info("scalabletrack: packet ", p.SequenceNumber, " is dropped because of currentSID < vp9Packet.SID")
		return
	}

	// mark packet as a last spatial layer packet
	if vp9Packet.E && currentSID == vp9Packet.SID && targetSID <= currentSID {
		p.Marker = true
	}

	t.send(p)
}

// functiont to normalize the sequence number in case the sequence is rollover
func normalizeSequenceNumber(sequence, drop uint16) uint16 {
	if sequence > drop {
		return sequence - drop
	} else {
		return 65535 - drop + sequence
	}
}

func (t *scaleableClientTrack) send(p *rtp.Packet) {
	p.Header.SequenceNumber = normalizeSequenceNumber(p.SequenceNumber, t.dropCounter)

	t.lastTimestamp = p.Timestamp

	// detect late packet
	if IsRTPPacketLate(p.Header.SequenceNumber, t.lastSequence) {
		glog.Warning("scalabletrack: packet SSRC ", t.remoteTrack.track.SSRC(), " client ", t.client.ID(), " sequence ", p.SequenceNumber, " is late, last sequence ", t.lastSequence)
		// return
	}

	t.lastSequence = p.Header.SequenceNumber

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
