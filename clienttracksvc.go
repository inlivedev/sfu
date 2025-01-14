package sfu

import (
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

var DefaultQualityPresets = map[QualityLevel]QualityPreset{
	QualityHigh: QualityPreset{
		SID: 2,
		TID: 2,
	},
	QualityHighMid: QualityPreset{
		SID: 2,
		TID: 1,
	},
	QualityHighLow: QualityPreset{
		SID: 2,
		TID: 0,
	},
	QualityMid: QualityPreset{
		SID: 1,
		TID: 2,
	},
	QualityMidMid: QualityPreset{
		SID: 1,
		TID: 1,
	},
	QualityMidLow: QualityPreset{
		SID: 1,
		TID: 0,
	},
	QualityLow: QualityPreset{
		SID: 0,
		TID: 2,
	},
	QualityLowMid: QualityPreset{
		SID: 0,
		TID: 1,
	},
	QualityLowLow: QualityPreset{
		SID: 0,
		TID: 0,
	},
	QualityNone: QualityPreset{
		SID: 0,
		TID: 0,
	},
}

func (q QualityPreset) GetSID() uint8 {
	return q.SID
}

func (q QualityPreset) GetTID() uint8 {
	return q.TID
}

type QualityPresets struct {
	High    QualityPreset `json:"high"`
	HighMid QualityPreset `json:"highmid"`
	HighLow QualityPreset `json:"highlow"`
	Mid     QualityPreset `json:"mid"`
	MidMid  QualityPreset `json:"midmid"`
	MidLow  QualityPreset `json:"midlow"`
	Low     QualityPreset `json:"low"`
	LowMid  QualityPreset `json:"lowmid"`
	LowLow  QualityPreset `json:"lowlow"`
}

func DefaultQualityLevels() []QualityLevel {
	return []QualityLevel{
		QualityHigh,
		QualityMid,
		QualityLow,
		QualityLowMid,
		QualityLowLow,
	}
}

type scaleableClientTrack struct {
	*clientTrack
	lastQuality   QualityLevel
	maxQuality    QualityLevel
	tid           uint8
	sid           uint8
	lastTimestamp uint32
	lastSequence  uint16
	init          bool
}

func newScaleableClientTrack(
	c *Client,
	t *Track,
) *scaleableClientTrack {

	sct := &scaleableClientTrack{
		clientTrack: newClientTrack(c, t, false, nil),
		maxQuality:  QualityHigh,
		lastQuality: QualityHigh,
	}

	sct.SetMaxQuality(QualityHigh)

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

func (t *scaleableClientTrack) getQuality() QualityLevel {
	claim := t.client.bitrateController.GetClaim(t.ID())

	if claim == nil {
		t.client.log.Warnf("scalabletrack: claim is nil")
		return QualityNone
	}

	return min(t.MaxQuality(), claim.Quality(), Uint32ToQualityLevel(t.client.quality.Load()))
}

func (t *scaleableClientTrack) push(p *rtp.Packet, _ QualityLevel) {

	vp9Packet := &codecs.VP9Packet{}
	if _, err := vp9Packet.Unmarshal(p.Payload); err != nil {
		_ = t.packetmap.Drop(p.SequenceNumber, vp9Packet.PictureID)

		return
	}

	quality := t.getQuality()

	qualityPreset := qualityLevelToPreset(quality)

	targetSID := qualityPreset.GetSID()
	targetTID := qualityPreset.GetTID()

	if !t.init {
		t.init = true
		t.sid = targetSID
		t.tid = targetTID
	}

	t.lastSequence = p.Header.SequenceNumber

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
		t.setLastQuality(quality)
	}

	sidNonReference := (p.Payload[0] & 0x01) != 0
	if currentTID < vp9Packet.TID || currentSID < vp9Packet.SID || (vp9Packet.SID > currentSID && sidNonReference) {
		// t.client.log.Infof("scalabletrack: packet ", p.SequenceNumber, " is dropped because of currentTID ", currentTID, "  < vp9Packet.TID", vp9Packet.TID)
		ok := t.packetmap.Drop(p.SequenceNumber, vp9Packet.PictureID)
		if ok {
			return
		}
	}

	// mark packet as a last spatial layer packet
	if vp9Packet.E && currentSID == vp9Packet.SID && targetSID <= currentSID {
		p.Marker = true
	}

	ok, newseqno, piddelta := t.packetmap.Map(p.SequenceNumber, vp9Packet.PictureID)
	if !ok {
		return
	}

	marker := (p.Payload[1] & 0x80) != 0
	if marker && newseqno == p.SequenceNumber && piddelta == 0 {
		t.send(p)
		return
	}

	if marker {
		p.Payload[1] |= 0x80
	}

	p.SequenceNumber = newseqno

	// if quality is none we need to send blank frame
	// make sure the player is paused when the quality is none.
	// quality none only possible when the video is not displayed
	if quality == QualityNone {
		if ok := t.packetmap.Drop(p.SequenceNumber, vp9Packet.PictureID); ok {
			return
		}

		p.Payload = p.Payload[:0]
	}

	t.send(p)
}

func (t *scaleableClientTrack) send(p *rtp.Packet) {
	t.mu.Lock()
	t.lastTimestamp = p.Timestamp
	t.mu.Unlock()

	if err := t.localTrack.WriteRTP(p); err != nil {
		t.client.log.Errorf("scaleabletrack: error on write rtp", err)
	}

}

func (t *scaleableClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen = (sourceType == TrackTypeScreen)
}

func (t *scaleableClientTrack) setLastQuality(quality QualityLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastQuality = quality
}

func (t *scaleableClientTrack) Quality() QualityLevel {
	return t.LastQuality()
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

	claim := t.Client().bitrateController.GetClaim(t.ID())
	if claim != nil {
		if claim.Quality() > quality && quality != QualityNone {
			claim.SetQuality(quality)
		}
	}

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
	go t.remoteTrack.SendPLI()
}
