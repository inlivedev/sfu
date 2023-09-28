package sfu

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type iClientTrack interface {
	getCurrentBitrate() uint32
	push(rtp *rtp.Packet, quality QualityLevel)
	ID() string
	Kind() webrtc.RTPCodecType
	LocalTrack() *webrtc.TrackLocalStaticRTP
	IsScreen() bool
	SetSourceType(TrackType)
	OnTrackEnded(func())
}

type ClientTrack struct {
	id                    string
	mu                    sync.RWMutex
	client                *Client
	kind                  webrtc.RTPCodecType
	mimeType              string
	localTrack            *webrtc.TrackLocalStaticRTP
	remoteTrack           *RemoteTrack
	isScreen              *atomic.Bool
	onTrackEndedCallbacks []func()
}

func (t *ClientTrack) ID() string {
	return t.id
}

func (t *ClientTrack) Kind() webrtc.RTPCodecType {
	return t.remoteTrack.track.Kind()
}

func (t *ClientTrack) push(rtp *rtp.Packet, quality QualityLevel) {
	if t.client.peerConnection.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	if err := t.localTrack.WriteRTP(rtp); err != nil {
		glog.Error("clienttrack: error on write rtp", err)
	}
}

func (t *ClientTrack) getCurrentBitrate() uint32 {
	return t.remoteTrack.GetCurrentBitrate()
}

func (t *ClientTrack) LocalTrack() *webrtc.TrackLocalStaticRTP {
	return t.localTrack
}

func (t *ClientTrack) IsScreen() bool {
	return t.isScreen.Load()
}

func (t *ClientTrack) SetSourceType(sourceType TrackType) {
	t.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *ClientTrack) OnTrackEnded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onTrackEndedCallbacks = append(t.onTrackEndedCallbacks, callback)
}

func (t *ClientTrack) onTrackEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.onTrackEndedCallbacks {
		callback()
	}
}

type SimulcastClientTrack struct {
	id                    string
	mu                    sync.RWMutex
	client                *Client
	kind                  webrtc.RTPCodecType
	mimeType              string
	localTrack            *webrtc.TrackLocalStaticRTP
	remoteTrack           *SimulcastTrack
	sequenceNumber        *atomic.Uint32
	lastQuality           *atomic.Uint32
	lastTimestamp         *atomic.Uint32
	lastCheckQualityTS    *atomic.Int64
	isScreen              *atomic.Bool
	isEnded               *atomic.Bool
	onTrackEndedCallbacks []func()
}

// func (t *SimulcastClientTrack) getAllowedQuality(kind webrtc.RTPCodecType) QualityLevel {
// 	var estimatedQuality QualityLevel
// 	var currentHighBitrate, currentMidBitrate, currentLowBitrate, maxAllowedBitrate uint32

// 	if kind == webrtc.RTPCodecTypeAudio {
// 		return QualityHigh
// 	}

// 	_, maxAllowedScreenBitrate, maxAllowedVideoBitrate := t.client.GetMaxBitratePerTrack()

// 	if t.isScreen.Load() {
// 		maxAllowedBitrate = maxAllowedScreenBitrate
// 	} else {
// 		maxAllowedBitrate = maxAllowedVideoBitrate
// 	}

// 	if t.remoteTrack.isTrackActive(QualityHigh) {
// 		currentHighBitrate = t.remoteTrack.remoteTrackHigh.GetCurrentBitrate()
// 	} else {
// 		currentHighBitrate = highBitrate
// 	}

// 	if t.remoteTrack.isTrackActive(QualityMid) {
// 		currentMidBitrate = t.remoteTrack.remoteTrackMid.GetCurrentBitrate()
// 	} else {
// 		currentMidBitrate = midBitrate
// 	}

// 	if t.remoteTrack.isTrackActive(QualityLow) {
// 		currentLowBitrate = t.remoteTrack.remoteTrackLow.GetCurrentBitrate()
// 	} else {
// 		currentLowBitrate = lowBitrate
// 	}

// 	if currentHighBitrate != 0 && maxAllowedBitrate >= currentHighBitrate {
// 		estimatedQuality = QualityHigh
// 	} else if currentMidBitrate != 0 && maxAllowedBitrate < currentHighBitrate && maxAllowedBitrate >= currentMidBitrate {
// 		estimatedQuality = QualityMid
// 	} else if currentLowBitrate != 0 && maxAllowedBitrate < currentMidBitrate && maxAllowedBitrate >= currentLowBitrate {
// 		estimatedQuality = QualityLow
// 	} else {
// 		estimatedQuality = QualityNone
// 		glog.Warning("track: no quality level is fit into the current bandwidth,  max allowed bitrate: ", maxAllowedBitrate)
// 		glog.Warning("track: current high bitrate ", currentHighBitrate, ", current mid bitrate: ", currentMidBitrate, ", current low bitrate: ", currentLowBitrate)
// 		glog.Warning("track: client ", t.client.ID(), " bandwidth ", t.client.GetEstimatedBandwidth())
// 	}

// 	clientQuality := Uint32ToQualityLevel(t.client.quality.Load())
// 	if clientQuality != 0 && estimatedQuality > clientQuality {
// 		return clientQuality
// 	}

// 	return estimatedQuality
// }

// TODO: change to bandwidth controller
func (t *SimulcastClientTrack) GetQuality() QualityLevel {
	track := t.remoteTrack

	var quality QualityLevel

	t.lastCheckQualityTS.Store(time.Now().UnixNano())
	availableBandwidth := t.client.bitrateController.GetAvailableBandwidth()

	if !t.client.bitrateController.Exist(t.ID()) {
		// new track

		quality = t.getDistributedQuality(availableBandwidth)

		claim, err := t.client.bitrateController.AddClaim(t, quality)
		if err != nil && err == ErrAlreadyClaimed {
			if err != nil {
				glog.Error("clienttrack: error on add claim ", err)
			}

			quality = QualityLevel(t.lastQuality.Load())
		} else if !claim.active {
			glog.Warning("clienttrack: claim is not active, claim bitrate ", ThousandSeparator(int(claim.bitrate)), " current bitrate ", ThousandSeparator(int(t.client.bitrateController.TotalBitrates(false))), " available bandwidth ", ThousandSeparator(int(availableBandwidth)))
			quality = QualityNone
		}

	} else {
		// check if the bitrate can be adjusted
		quality = t.client.bitrateController.getNextTrackQuality(t.ID())
	}

	clientQuality := Uint32ToQualityLevel(t.client.quality.Load())
	if clientQuality != 0 && quality > clientQuality {
		quality = clientQuality
	}

	lastQuality := t.LastQuality()
	if !track.isTrackActive(quality) {
		if !track.isTrackActive(lastQuality) {
			return QualityNone
		}

		return lastQuality
	}

	return quality
}

func (t *SimulcastClientTrack) push(rtp *rtp.Packet, quality QualityLevel) {

	var trackQuality QualityLevel

	lastQuality := t.LastQuality()

	if t.client.peerConnection.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}

	isKeyframe := IsKeyframe(t.mimeType, rtp)
	// lastCheckQualityDuration := time.Since(time.Unix(0, t.lastCheckQualityTS.Load()))

	// prevent the packet to be written to the new local track if the packet is not a keyframe
	// this is to avoid broken or froze video on client side
	if !isKeyframe && lastQuality == 0 {
		t.remoteTrack.sendPLI(trackQuality)
		return
	}

	if isKeyframe && t.lastTimestamp.Load() != rtp.Timestamp { // && lastCheckQualityDuration.Seconds() >= 1 {
		trackQuality = t.GetQuality()
		if trackQuality == QualityNone {
			t.lastQuality.Store(uint32(trackQuality))
			return
		}
	} else {
		trackQuality = lastQuality
	}

	if trackQuality == quality {
		// set the last processed packet timestamp to identify if is begining of the new frame
		t.lastTimestamp.Store(rtp.Timestamp)
		// make sure the timestamp and sequence number is consistent from the previous packet even it is not the same track

		// credit to https://github.com/k0nserv for helping me with this on Pion Slack channel
		switch quality {
		case QualityHigh:
			rtp.Timestamp = t.remoteTrack.baseTS + ((rtp.Timestamp - t.remoteTrack.remoteTrackHighBaseTS) - t.remoteTrack.remoteTrackHighBaseTS)
		case QualityMid:
			rtp.Timestamp = t.remoteTrack.baseTS + ((rtp.Timestamp - t.remoteTrack.remoteTrackMidBaseTS) - t.remoteTrack.remoteTrackMidBaseTS)
		case QualityLow:
			rtp.Timestamp = t.remoteTrack.baseTS + ((rtp.Timestamp - t.remoteTrack.remoteTrackLowBaseTS) - t.remoteTrack.remoteTrackLowBaseTS)
		}

		t.sequenceNumber.Add(1)
		rtp.SequenceNumber = uint16(t.sequenceNumber.Load())

		if lastQuality != quality {
			t.lastQuality.Store(uint32(quality))
		}

		t.mu.Lock()
		defer t.mu.Unlock()

		if err := t.localTrack.WriteRTP(rtp); err != nil {
			glog.Error("track: error on write rtp", err)
		}
	}
}

func (t *SimulcastClientTrack) GetRemoteTrack() *RemoteTrack {
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

func (t *SimulcastClientTrack) getDistributedQuality(availableBandwidth uint32) QualityLevel {
	audioTracksCount := 0
	videoTracksCount := 0
	simulcastTracksCount := 0

	clients := t.client.sfu.clients.GetClients()

	for _, client := range clients {
		if t.client.ID() != client.ID() {
			for _, track := range client.tracks.GetTracks() {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					audioTracksCount++
				} else {
					if track.IsSimulcast() {
						simulcastTracksCount++
					} else {
						videoTracksCount++
					}
				}
			}
		}
	}

	leftBandwidth := availableBandwidth - (uint32(audioTracksCount) * t.client.sfu.bitratesConfig.Audio) - (uint32(videoTracksCount) * t.client.sfu.bitratesConfig.Video)

	distributedBandwidth := leftBandwidth / uint32(simulcastTracksCount)

	if distributedBandwidth > t.client.sfu.bitratesConfig.VideoHigh {
		return QualityHigh
	} else if distributedBandwidth < t.client.sfu.bitratesConfig.VideoHigh && distributedBandwidth > t.client.sfu.bitratesConfig.VideoMid {
		return QualityMid
	} else {
		return QualityLow
	}
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
