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
	// According to VP9 RFC, B bit set to 1 if packet is the beginning of a new VP9 frame
	// For keyframes, P bit should be 0 (no inter-picture prediction)
	if !vp9.B {
		return false
	}

	// Check for keyframe by examining the VP9 payload
	// This identifies the frame as an intra-frame (keyframe)
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

	// Temporal layer switching according to VP9 RFC
	// U bit indicates a switching up point where we can safely switch to a higher frame rate
	if t.tid < targetTID {
		if vp9Packet.U && vp9Packet.B && currentTID < vp9Packet.TID && vp9Packet.TID <= targetTID {
			// Scale temporal up - U bit confirms it's safe to switch up
			t.tid = vp9Packet.TID
			currentTID = t.tid
		}
	} else if t.tid > targetTID {
		if vp9Packet.E {
			// Scale temporal down - safe at the end of a frame
			t.tid = vp9Packet.TID
		}
	}

	// Spatial layer switching according to VP9 RFC
	// D bit indicates inter-layer dependency
	if currentSID < targetSID {
		// Switching up to higher spatial layer
		// For scaling up, we should ensure this is the start of a frame (B=1)
		// and the layer doesn't depend on layers we might have skipped
		if vp9Packet.B && currentSID < vp9Packet.SID && vp9Packet.SID <= targetSID {
			// For non-base layers, check the D bit to understand dependencies
			if vp9Packet.SID > 0 && vp9Packet.D {
				// This layer depends on the previous spatial layer
				// Only switch if we have the previous layer
				if vp9Packet.SID == currentSID+1 {
					t.sid = vp9Packet.SID
					currentSID = t.sid
				}
			} else {
				// This layer doesn't depend on previous spatial layer (D=0) or is base layer
				// Safe to switch to this layer
				t.sid = vp9Packet.SID
				currentSID = t.sid
			}
		}
	} else if currentSID > targetSID {
		// Switching down to lower spatial layer
		// Safe to switch down at the end of a frame
		if vp9Packet.E {
			t.sid = vp9Packet.SID
		}
	}

	if vp9Packet.E && t.tid == targetTID && t.sid == targetSID {
		t.setLastQuality(quality)
	}

	// Determine if packet should be dropped
	shouldDrop := false

	// Drop packets higher than our target temporal layer
	if vp9Packet.TID > currentTID {
		shouldDrop = true
	}

	// Drop packets higher than our target spatial layer
	if vp9Packet.SID > currentSID {
		shouldDrop = true
	}

	// Drop packets from non-reference frames for higher spatial layers that we're not using
	// Similar to Z bit concept in the RFC
	sidNonReference := (p.Payload[0] & 0x01) != 0
	if vp9Packet.SID > currentSID && sidNonReference {
		shouldDrop = true
	}

	if shouldDrop {
		ok := t.packetmap.Drop(p.SequenceNumber, vp9Packet.PictureID)
		if ok {
			return
		}
	}

	// Mark packet as a last spatial layer packet
	// According to RFC: Marker bit (M) MUST be set to 1 for the final packet of the
	// highest spatial layer frame (the final packet of the picture)
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

	// If quality is none we need to send blank frame
	// Make sure the player is paused when the quality is none.
	// Quality none only possible when the video is not displayed
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
