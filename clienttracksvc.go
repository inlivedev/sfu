package sfu

import (
	"context"
	"sync"
	"time"

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

type scaleableClientTrack struct {
	id            string
	context       context.Context
	cancel        context.CancelFunc
	mu            sync.RWMutex
	client        *Client
	kind          webrtc.RTPCodecType
	mimeType      string
	localTrack    *webrtc.TrackLocalStaticRTP
	remoteTrack   *Track
	lastQuality   QualityLevel
	maxQuality    QualityLevel
	tid           uint8
	sid           uint8
	lastTimestamp uint32
	isScreen      bool
	dropCounter   uint16
	qualityPreset QualityPreset
	packetCaches  *packetCaches
	packetChan    chan *rtp.Packet
}

func newScaleableClientTrack(
	c *Client,
	t *Track,
	qualityPreset QualityPreset,
) *scaleableClientTrack {
	ctx, cancel := context.WithCancel(t.Context())

	sct := &scaleableClientTrack{
		context:       ctx,
		cancel:        cancel,
		mu:            sync.RWMutex{},
		id:            t.base.id,
		kind:          t.base.kind,
		mimeType:      t.base.codec.MimeType,
		client:        c,
		localTrack:    t.createLocalTrack(),
		remoteTrack:   t,
		isScreen:      t.IsScreen(),
		qualityPreset: qualityPreset,
		maxQuality:    QualityHigh,
		lastQuality:   QualityHigh,
		packetCaches:  newPacketCaches(100 * time.Millisecond),
		packetChan:    make(chan *rtp.Packet, 1),
		tid:           qualityPreset.High.TID,
		sid:           qualityPreset.High.SID,
	}

	return sct
}

func (t *scaleableClientTrack) Client() *Client {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.client
}

func (t *scaleableClientTrack) Context() context.Context {
	return t.context
}

func (t *scaleableClientTrack) writeRTP(p *rtp.Packet) {
	t.lastTimestamp = p.Timestamp

	if err := t.localTrack.WriteRTP(p); err != nil {
		glog.Error("track: error on write rtp", err)
	}
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

// this where the temporal and spatial layers are will be decided to be sent to the client or not
// compare it with the claimed quality to decide if the packet should be sent or not
func (t *scaleableClientTrack) push(p *rtp.Packet, _ QualityLevel) {
	packets, err := t.packetCaches.Sort(p)
	if err == ErrPacketTooLate {
		glog.Warning("scalabletrack: ", err)
	}

	for _, pkt := range packets {
		t.process(pkt)
	}
}

func (t *scaleableClientTrack) process(p *rtp.Packet) {
	var qualityPreset IQualityPreset

	vp9Packet := &codecs.VP9Packet{}
	if _, err := vp9Packet.Unmarshal(p.Payload); err != nil {
		t.send(p)
		return
	}

	isKeyframe := t.isKeyframe(vp9Packet)
	if isKeyframe {
		glog.Info("scalabletrack: keyframe ", p.SequenceNumber, " is detected")
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
	p.SequenceNumber = normalizeSequenceNumber(p.SequenceNumber, t.dropCounter)

	t.writeRTP(p)
}

func (t *scaleableClientTrack) RemoteTrack() *remoteTrack {
	return t.remoteTrack.remoteTrack
}

func (t *scaleableClientTrack) ID() string {
	return t.id
}

func (t *scaleableClientTrack) Kind() webrtc.RTPCodecType {
	return t.kind
}

func (t *scaleableClientTrack) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *scaleableClientTrack) IsScreen() bool {
	return t.isScreen
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

	t.RemoteTrack().sendPLI()
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
	t.remoteTrack.remoteTrack.sendPLI()
}

func (t *scaleableClientTrack) getQuality() QualityLevel {
	claim := t.client.bitrateController.GetClaim(t.ID())

	if claim == nil {
		glog.Warning("scalabletrack: claim is nil")
		return QualityNone
	}

	return min(t.MaxQuality(), claim.Quality(), Uint32ToQualityLevel(t.client.quality.Load()))
}
