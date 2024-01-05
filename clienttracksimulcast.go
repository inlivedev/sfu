package sfu

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type simulcastClientTrack struct {
	id                      string
	mu                      sync.RWMutex
	client                  *Client
	context                 context.Context
	cancel                  context.CancelFunc
	kind                    webrtc.RTPCodecType
	mimeType                string
	localTrack              *webrtc.TrackLocalStaticRTP
	remoteTrack             *SimulcastTrack
	lastBlankSequenceNumber *atomic.Uint32
	sequenceNumber          *atomic.Uint32
	lastQuality             *atomic.Uint32
	paddingTS               *atomic.Uint32
	maxQuality              *atomic.Uint32
	lastTimestamp           *atomic.Uint32
	isScreen                *atomic.Bool
	isEnded                 *atomic.Bool
	onTrackEndedCallbacks   []func()
}

func (t *simulcastClientTrack) Client() *Client {
	return t.client
}

func (t *simulcastClientTrack) Context() context.Context {
	return t.context
}

func (t *simulcastClientTrack) isFirstKeyframePacket(p rtp.Packet) bool {
	isKeyframe := IsKeyframe(t.mimeType, p)

	return isKeyframe && t.lastTimestamp.Load() != p.Timestamp
}

func (t *simulcastClientTrack) send(p rtp.Packet, quality QualityLevel, lastQuality QualityLevel) {
	t.lastTimestamp.Store(p.Timestamp)

	if lastQuality != quality {
		t.lastQuality.Store(uint32(quality))
	}

	p = t.rewritePacket(p, quality)

	t.writeRTP(p)

}

func (t *simulcastClientTrack) writeRTP(p rtp.Packet) {
	if err := t.localTrack.WriteRTP(&p); err != nil {
		glog.Error("track: error on write rtp", err)
	}
}

func (t *simulcastClientTrack) push(p rtp.Packet, quality QualityLevel) {
	var trackQuality QualityLevel

	lastQuality := t.LastQuality()

	if !t.client.bitrateController.exists(t.ID()) {
		// do nothing if the bitrate claim is not exist
		return
	}

	isFirstKeyframePacket := t.isFirstKeyframePacket(p)
	if isFirstKeyframePacket {
		t.remoteTrack.KeyFrameReceived()
	}

	// check if it's a first packet to send
	if lastQuality == QualityNone && t.sequenceNumber.Load() == 0 {
		// we try to send the low quality first	if the track is active and fallback to upper quality if not
		if t.remoteTrack.getRemoteTrack(QualityLow) != nil && quality == QualityLow {
			trackQuality = QualityLow
			t.lastQuality.Store(uint32(QualityLow))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI(QualityLow)
		} else if t.remoteTrack.getRemoteTrack(QualityMid) != nil && quality == QualityMid {
			trackQuality = QualityMid
			t.lastQuality.Store(uint32(QualityMid))
			// send PLI to make sure the client will receive the first frame
			t.remoteTrack.sendPLI(QualityMid)
		} else if t.remoteTrack.getRemoteTrack(QualityHigh) != nil && quality == QualityHigh {
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
			trackQuality = t.client.bitrateController.getQuality(t)
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

			t.lastQuality.Store(uint32(trackQuality))
		}
	}

	if trackQuality == quality {
		t.send(p, trackQuality, lastQuality)
	}
}

func (t *simulcastClientTrack) GetRemoteTrack() *remoteTrack {
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

func (t *simulcastClientTrack) getCurrentBitrate() uint32 {
	currentTrack := t.GetRemoteTrack()
	if currentTrack == nil {
		return 0
	}

	return currentTrack.GetCurrentBitrate()
}

func (t *simulcastClientTrack) ID() string {
	return t.id
}

func (t *simulcastClientTrack) Kind() webrtc.RTPCodecType {
	return t.kind
}

func (t *simulcastClientTrack) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *simulcastClientTrack) IsScreen() bool {
	return t.isScreen.Load()
}

func (t *simulcastClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *simulcastClientTrack) LastQuality() QualityLevel {
	return Uint32ToQualityLevel(t.lastQuality.Load())
}

func (t *simulcastClientTrack) OnTrackEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, callback)
}

func (t *simulcastClientTrack) onTrackEnded() {
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

func (t *simulcastClientTrack) SetMaxQuality(quality QualityLevel) {
	t.maxQuality.Store(uint32(quality))
}

func (t *simulcastClientTrack) MaxQuality() QualityLevel {
	return Uint32ToQualityLevel(t.maxQuality.Load())
}

func (t *simulcastClientTrack) IsSimulcast() bool {
	return true
}

func (t *simulcastClientTrack) IsScaleable() bool {
	return false
}

func (t *simulcastClientTrack) rewritePacket(p rtp.Packet, quality QualityLevel) rtp.Packet {
	t.remoteTrack.mu.Lock()
	defer t.remoteTrack.mu.Unlock()
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

	return p
}

func (t *simulcastClientTrack) RequestPLI() {
	t.remoteTrack.sendPLI(t.LastQuality())
}
