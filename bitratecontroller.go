package sfu

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
)

var (
	ErrAlreadyClaimed          = errors.New("bwcontroller: already claimed")
	ErrorInsufficientBandwidth = errors.New("bwcontroller: bandwidth is insufficient")
)

const (
	keepBitrate     = 0
	increaseBitrate = 1
	decreaseBitrate = 2
)

type bitrateAdjustment int

type bitrateClaim struct {
	track     iClientTrack
	bitrate   uint32
	quality   QualityLevel
	simulcast bool
}

type packetMonitor struct {
	state               int
	previousSentPackets uint32
	previousLostPackets uint32
}

type bitrateController struct {
	mu                   sync.RWMutex
	client               *Client
	claims               map[string]*bitrateClaim
	packetMonitor        packetMonitor
	packetLossPercentage uint8
}

func newbitrateController(client *Client, intervalMonitor time.Duration) *bitrateController {
	bc := &bitrateController{
		mu:            sync.RWMutex{},
		client:        client,
		claims:        make(map[string]*bitrateClaim, 0),
		packetMonitor: packetMonitor{},
	}

	bc.start()

	return bc
}

func (bc *bitrateController) Claims() map[string]*bitrateClaim {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	claims := make(map[string]*bitrateClaim, 0)
	for k, v := range bc.claims {
		claims[k] = v
	}

	return claims
}

func (bc *bitrateController) Exist(id string) bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; ok {
		return true
	}

	return false
}

func (bc *bitrateController) GetClaim(id string) *bitrateClaim {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if claim, ok := bc.claims[id]; ok {
		return claim
	}

	return nil
}

// if all is false, only active claims will be counted
func (bc *bitrateController) TotalBitrates() uint32 {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	return bc.totalBitrates()
}

func (bc *bitrateController) totalBitrates() uint32 {
	total := uint32(0)
	for _, claim := range bc.claims {
		total += claim.bitrate
	}

	return total
}

func (bc *bitrateController) setQuality(clientTrackID string, quality QualityLevel) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if claim, ok := bc.claims[clientTrackID]; ok {
		bitrate := bc.client.sfu.QualityLevelToBitrate(quality)
		claim.quality = quality
		claim.bitrate = bitrate
		bc.claims[clientTrackID] = claim
	}
}

func (bc *bitrateController) setSimulcastClaim(clientTrackID string, simulcast bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if claim, ok := bc.claims[clientTrackID]; ok {
		claim.simulcast = simulcast
		bc.claims[clientTrackID] = claim
	}
}

// this handle some simulcast failed to send mid and low track, only high track available
// by default we just send the high track that is only available
func (bc *bitrateController) checkAllTrackActive(claim *bitrateClaim) (bool, QualityLevel) {
	trackCount := 0
	quality := QualityNone
	track, ok := claim.track.(*simulcastClientTrack)

	if ok {
		if track.remoteTrack.remoteTrackHigh != nil {
			trackCount++
			quality = QualityHigh
		}

		if track.remoteTrack.remoteTrackMid != nil {
			trackCount++
			quality = QualityMid
		}

		if track.remoteTrack.remoteTrackLow != nil {
			trackCount++
			quality = QualityLow
		}

		if trackCount == 1 {
			qualityLvl := Uint32ToQualityLevel(uint32(quality))
			if claim.quality != qualityLvl {
				bc.setQuality(claim.track.ID(), qualityLvl)
			}

			// this will force the current track identified as non simulcast track
			if claim.simulcast {
				bc.setSimulcastClaim(claim.track.ID(), false)
			}

			return true, qualityLvl
		}

		return true, claim.quality
	}

	return false, claim.quality
}

func (bc *bitrateController) addClaims(clientTracks []iClientTrack) error {
	distributedVideoQuality := Uint32ToQualityLevel(QualityNone)

	availableBandwidth := bc.client.GetEstimatedBandwidth() - bc.TotalBitrates()
	if availableBandwidth > 0 {
		distributedVideoQuality = bc.getDistributedQuality(int32(availableBandwidth))
	}

	bc.mu.Lock()
	defer bc.mu.Unlock()

	errors := make([]error, 0)

	for _, clientTrack := range clientTracks {
		trackQuality := distributedVideoQuality
		if _, ok := bc.claims[clientTrack.ID()]; ok {
			errors = append(errors, ErrAlreadyClaimed)
			continue
		}

		if clientTrack.Kind() == webrtc.RTPCodecTypeAudio {
			if clientTrack.LocalTrack().Codec().MimeType == "audio/RED" {
				trackQuality = QualityAudioRed
			} else {
				trackQuality = QualityAudio
			}
		}

		_, err := bc.addClaim(clientTrack, trackQuality, true)
		if err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		return FlattenErrors(errors)
	}

	return nil
}

func (bc *bitrateController) addClaim(clientTrack iClientTrack, quality QualityLevel, locked bool) (*bitrateClaim, error) {
	bitrate := bc.client.sfu.QualityLevelToBitrate(quality)

	if !locked {
		bc.mu.RLock()
		defer bc.mu.RUnlock()
	}

	bc.claims[clientTrack.ID()] = &bitrateClaim{
		track:     clientTrack,
		quality:   quality,
		simulcast: clientTrack.IsSimulcast(),
		bitrate:   bitrate,
	}

	clientTrack.OnTrackEnded(func() {
		bc.RemoveClaim(clientTrack.ID())
	})

	return bc.claims[clientTrack.ID()], nil
}

func (bc *bitrateController) RemoveClaim(id string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; !ok {
		return
	}

	delete(bc.claims, id)
}

// getAvailableBandwidth returns the available bandwidth for the client to claim
// The available bandwidth is the estimated bandwidth minus the total bitrate of all active claims
// Use this information for debugging or logging purpose
func (bc *bitrateController) availableBandwidth() int32 {
	estimatedBandwidth := int32(bc.client.GetEstimatedBandwidth())
	availableBandwidth := estimatedBandwidth - int32(bc.TotalBitrates())
	minSpace := int32(bc.client.sfu.bitratesConfig.VideoLow + bc.client.sfu.bitratesConfig.Audio)

	if estimatedBandwidth > minSpace {
		// always keep extra space for another 1 low video track and 1 audio track
		availableBandwidth -= minSpace
	}

	return availableBandwidth
}

func (bc *bitrateController) exists(id string) bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; ok {
		return true
	}

	return false
}

func (bc *bitrateController) isScreenNeedIncrease(highestQuality QualityLevel) bool {
	for _, claim := range bc.claims {
		if claim.track.IsScreen() && claim.quality <= highestQuality {
			return true
		}
	}

	return false
}

func (bc *bitrateController) isThereNonScreenCanDecrease(lowestQuality QualityLevel) bool {
	for _, claim := range bc.claims {
		if !claim.track.IsScreen() && claim.quality > lowestQuality {
			return true
		}
	}

	return false
}

func (bc *bitrateController) getQuality(t *simulcastClientTrack) QualityLevel {
	track := t.remoteTrack

	claim := bc.GetClaim(t.ID())
	if claim == nil {
		// this must be never reached
		panic("bitrate: claim is not exists")
	}

	quality := claim.quality

	maxQuality := t.getMaxQuality()
	if maxQuality < quality {
		quality = maxQuality
	}

	clientQuality := Uint32ToQualityLevel(t.client.quality.Load())
	if quality > clientQuality {
		quality = clientQuality
	}

	lastQuality := t.LastQuality()
	if quality != QualityNone && !track.isTrackActive(quality) {
		if lastQuality == QualityNone && track.isTrackActive(QualityLow) {
			glog.Warning("bitrate: send low quality because selected quality is not active and last quality is none")
			return QualityLow
		}

		if !track.isTrackActive(lastQuality) {
			glog.Warning("bitrate: quality ", quality, " and fallback quality ", lastQuality, " is not active, so sending no quality.")

			return QualityNone
		}

		glog.Warning("bitrate: quality ", quality, " is not active,  fallback to last quality ", lastQuality)

		return lastQuality
	}

	return quality
}

func (bc *bitrateController) getDistributedQuality(availableBandwidth int32) QualityLevel {
	audioTracksCount := 0
	audioRedCount := 0
	videoTracksCount := 0
	simulcastTracksCount := 0

	for _, track := range bc.client.publishedTracks.GetTracks() {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			if track.MimeType() == "audio/RED" {
				audioRedCount++
			} else {
				audioTracksCount++
			}
		} else {
			if track.IsSimulcast() {
				simulcastTracksCount++
			} else {
				videoTracksCount++
			}
		}
	}

	audioClaimsCount := 0
	videoClaimsCount := 0
	simulcastClaimsCount := 0

	highCount := 0
	midCount := 0
	lowCount := 0

	for _, claim := range bc.claims {
		if claim.track.Kind() == webrtc.RTPCodecTypeAudio {
			audioClaimsCount++
		} else {
			if claim.track.IsSimulcast() {
				simulcastClaimsCount++

				switch claim.quality {
				case QualityHigh:
					highCount++
				case QualityMid:
					midCount++
				case QualityLow:
					lowCount++
				}

			} else {
				videoClaimsCount++
			}
		}
	}

	leftBandwidth := availableBandwidth - (int32(audioTracksCount-audioClaimsCount) * int32(bc.client.sfu.bitratesConfig.Audio))
	leftBandwidth -= (int32(audioRedCount) * int32(bc.client.sfu.bitratesConfig.AudioRed))
	leftBandwidth -= (int32(videoTracksCount-videoClaimsCount) * int32(bc.client.sfu.bitratesConfig.Video))
	leftBandwidth -= (int32(simulcastTracksCount-highCount) * int32(bc.client.sfu.bitratesConfig.VideoHigh))
	leftBandwidth -= (int32(simulcastTracksCount-midCount) * int32(bc.client.sfu.bitratesConfig.VideoMid))
	leftBandwidth -= (int32(simulcastTracksCount-lowCount) * int32(bc.client.sfu.bitratesConfig.VideoLow))

	newTrackCount := int32(simulcastTracksCount - simulcastClaimsCount)

	if newTrackCount == 0 {
		// all tracks already claimed bitrates, return none
		return QualityNone
	}

	distributedBandwidth := int32(leftBandwidth) / newTrackCount

	if distributedBandwidth > int32(bc.client.sfu.bitratesConfig.VideoHigh) && highCount > 0 {
		return QualityHigh
	} else if distributedBandwidth < int32(bc.client.sfu.bitratesConfig.VideoHigh) && distributedBandwidth > int32(bc.client.sfu.bitratesConfig.VideoMid) && (midCount > 0 || highCount > 0) {
		return QualityMid
	} else {
		return QualityLow
	}
}

func (bc *bitrateController) totalSentBitrates() uint32 {
	total := uint32(0)
	bc.mu.Lock()
	defer bc.mu.Unlock()
	for _, claim := range bc.claims {
		total += claim.track.getCurrentBitrate()
	}

	return total
}

func (bc *bitrateController) start() {
	go func() {
		context, cancel := context.WithCancel(bc.client.context)
		defer cancel()

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-context.Done():
				return
			case <-ticker.C:
				bc.checkAndAdjustBitrates()
			}
		}
	}()
}

// checkAndAdjustBitrates will check if the available bandwidth is enough to send the current bitrate
// if not then it will try to reduce one by one of simulcast track quality until it fit the bandwidth
// if the bandwidth is enough to send the current bitrate, then it will try to increase the bitrate
func (bc *bitrateController) checkAndAdjustBitrates() {
	claims := bc.Claims()
	lowestQuality := QualityLevel(QualityHigh)
	highestQuality := QualityLevel(QualityNone)

	noneCount := 0
	lowCount := 0
	midCount := 0
	highCount := 0
	screenCount := 0

	for _, claim := range claims {
		allActive, quality := bc.checkAllTrackActive(claim)
		if !allActive {
			bc.setQuality(claim.track.ID(), quality)
		}

		if claim.quality < lowestQuality {
			lowestQuality = claim.quality
		}

		if claim.quality > highestQuality {
			highestQuality = claim.quality
		}

		if claim.track.IsScreen() {
			screenCount++
		}

		switch claim.quality {
		case QualityNone:
			noneCount++
		case QualityLow:
			lowCount++
		case QualityMid:
			midCount++
		case QualityHigh:
			highCount++
		}

	}

	bitrateAdjustment := bc.getBitrateAdjustment()

	switch bitrateAdjustment {
	case keepBitrate:

	case decreaseBitrate:
		// need reduce the bitrate
		for _, claim := range claims {
			if claim.track.IsSimulcast() && claim.quality > QualityNone {
				reducedQuality := claim.quality - 1

				if claim.quality == QualityLow && midCount+highCount > 0 {
					continue
				} else if claim.quality == QualityMid && highCount > 0 {
					continue
				}

				if claim.track.IsScreen() && reducedQuality == QualityNone {
					// never reduce screen track to none
					continue
				} else if claim.track.IsScreen() && bc.isThereNonScreenCanDecrease(lowestQuality) {
					// skip if there is a non screen track can be reduced
					continue
				}

				bc.setQuality(claim.track.ID(), reducedQuality)
			}
		}
	case increaseBitrate:
		for _, claim := range claims {
			if claim.track.IsSimulcast() && claim.quality < QualityHigh {
				increasedQuality := claim.quality + 1

				if claim.quality == QualityMid && noneCount+lowCount > 0 {
					continue
				} else if claim.quality == QualityLow && noneCount > 0 {
					continue
				}

				if !claim.track.IsScreen() && bc.isScreenNeedIncrease(highestQuality) {
					continue
				}

				bc.setQuality(claim.track.ID(), increasedQuality)
			}
		}
	}

}

func (bc *bitrateController) onRemoteViewedSizeChanged(videoSize videoSize) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	claim, ok := bc.claims[videoSize.TrackID]
	if !ok {
		glog.Error("bitrate: track ", videoSize.TrackID, " is not exists")
		return
	}

	if claim.track.Kind() != webrtc.RTPCodecTypeVideo {
		glog.Error("bitrate: track ", videoSize.TrackID, " is not video track")
		return
	}

	simulcastTrack, ok := claim.track.(*simulcastClientTrack)
	if !ok {
		glog.Error("bitrate: track ", videoSize.TrackID, " is not simulcast track")
		return
	}

	if videoSize.Width == 0 || videoSize.Height == 0 {
		simulcastTrack.setMaxQuality(QualityNone)
	}

	if videoSize.Width <= bc.client.sfu.bitratesConfig.VideoLowPixels {
		simulcastTrack.setMaxQuality(QualityLow)
	} else if videoSize.Width <= bc.client.sfu.bitratesConfig.VideoMidPixels {
		simulcastTrack.setMaxQuality(QualityMid)
	} else {
		simulcastTrack.setMaxQuality(QualityHigh)
	}
}

// This bitrate adjuster use packet loss ratio to adjust the bitrate
// It will check the sender packet loss and compare it with total sent packets to calculate the ratio
// Ratio 2-10% considereed as good and will keep the bitrate
// Ratio less than 2% will increase the bitrate
// Ratio more than 10% will decrease the bitrate
//
// Reference
// https://www.ietf.org/archive/id/draft-alvestrand-rtcweb-congestion-01.html#rfc.section.4
// https://source.chromium.org/chromium/chromium/src/+/main:third_party/webrtc/modules/congestion_controller/goog_cc/send_side_bandwidth_estimation.cc;l=52
func (bc *bitrateController) getBitrateAdjustment() bitrateAdjustment {
	packetLost := int64(0)
	packetSent := uint64(0)

	bc.client.stats.senderMu.Lock()
	for _, stat := range bc.client.stats.Sender {
		packetLost += stat.Stats.RemoteInboundRTPStreamStats.PacketsLost
		packetSent += stat.Stats.OutboundRTPStreamStats.PacketsSent
	}

	bc.client.stats.senderMu.Unlock()

	if bc.packetMonitor.previousSentPackets == 0 {
		// first time, just set the previous value
		bc.packetMonitor.previousSentPackets = uint32(packetLost)
		bc.packetMonitor.previousSentPackets = uint32(packetSent)
		return keepBitrate
	}

	deltaSentPackets := uint32(packetSent) - bc.packetMonitor.previousSentPackets
	deltaLostPackets := uint32(packetLost) - bc.packetMonitor.previousLostPackets

	if bc.packetLossPercentage != 0 && float32(deltaLostPackets) < float32(bc.packetLossPercentage)/100 {
		deltaLostPackets = uint32(math.Floor(float64(deltaSentPackets) * float64(bc.packetLossPercentage) / float64(100)))
	}

	if deltaLostPackets == 0 {
		// avoid devide by zero
		return increaseBitrate
	}

	if deltaSentPackets == 0 {
		// avoid devide by zero
		return keepBitrate
	}

	ratio := float64(deltaLostPackets) / float64(deltaSentPackets)

	bc.packetMonitor.previousSentPackets = uint32(packetLost)
	bc.packetMonitor.previousSentPackets = uint32(packetSent)

	if ratio < 0.02 {
		// increase bitrate
		glog.Info("bitrate: client ", bc.client.id, " packet lost ratio ", ratio, " is less than 2%, increasing bitrate")
		bc.packetMonitor.state = increaseBitrate
		return increaseBitrate
	} else if ratio > 0.1 {
		// decrease bitrate
		glog.Info("bitrate: client ", bc.client.id, " packet lost ratio ", ratio, " is more than 10%, decreasing bitrate")
		bc.packetMonitor.state = decreaseBitrate
		return decreaseBitrate
	}

	// keep the bitrate
	glog.Info("bitrate: client ", bc.client.id, " packet lost ratio ", ratio, " is between 2-10%, keeping bitrate")
	bc.packetMonitor.state = keepBitrate
	return keepBitrate
}
