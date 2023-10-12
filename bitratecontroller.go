package sfu

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
)

var (
	ErrAlreadyClaimed          = errors.New("bwcontroller: already claimed")
	ErrorInsufficientBandwidth = errors.New("bwcontroller: bandwidth is insufficient")
)

type bitrateClaim struct {
	track     iClientTrack
	bitrate   uint32
	quality   QualityLevel
	simulcast bool
}

type bitrateController struct {
	mu     sync.RWMutex
	client *Client
	claims map[string]*bitrateClaim
}

func newbitrateController(client *Client, intervalMonitor time.Duration) *bitrateController {
	bc := &bitrateController{
		mu:     sync.RWMutex{},
		client: client,
		claims: make(map[string]*bitrateClaim, 0),
	}

	bc.start(intervalMonitor)

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
	track, ok := claim.track.(*SimulcastClientTrack)

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

func (bc *bitrateController) getQuality(t *SimulcastClientTrack) QualityLevel {
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

func (bc *bitrateController) start(interval time.Duration) {
	go func() {
		context, cancel := context.WithCancel(bc.client.context)
		defer cancel()

		ticker := time.NewTicker(interval)
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
	availableFreeBandwidth := bc.availableBandwidth()
	estimatedBandwidth := bc.client.GetEstimatedBandwidth()

	claims := bc.Claims()

	for _, claim := range claims {
		allActive, quality := bc.checkAllTrackActive(claim)
		if !allActive {
			bc.setQuality(claim.track.ID(), quality)
		}
	}

	if availableFreeBandwidth < 0 {
		// need reduce the bitrate
		for _, claim := range claims {
			if claim.track.IsSimulcast() && claim.quality > QualityNone {
				reducedQuality := claim.quality - 1

				if claim.track.IsScreen() && reducedQuality == QualityNone {
					// never reduce screen track to none
					continue
				}

				glog.Info("bitrate: bandwidth ", ThousandSeparator(int(estimatedBandwidth)), " is not enough to send ", ThousandSeparator(int(bc.TotalBitrates())), " bitrate, reducing quality to ", reducedQuality)
				additionalBitrate := bc.calculateDeltaBitrate(claim.quality, reducedQuality)
				availableFreeBandwidth += additionalBitrate
				bc.setQuality(claim.track.ID(), reducedQuality)

				if availableFreeBandwidth > 0 {
					// enough bandwidth, return
					return
				}
			}
		}
	}

	if availableFreeBandwidth > 0 || availableFreeBandwidth < 0 && estimatedBandwidth < 150_000 {
		// possible to increase the bitrate

		if estimatedBandwidth < 150_000 {
			// this to test if the actual bandwidth possible to send more than estimated bandwidth
			availableFreeBandwidth = 200_000
		}

		for _, claim := range claims {
			if claim.track.IsSimulcast() && claim.quality < QualityHigh {
				increasedQuality := claim.quality + 1
				additionalBitrate := bc.calculateDeltaBitrate(claim.quality, increasedQuality)
				availableFreeBandwidth += additionalBitrate
				if availableFreeBandwidth < 0 {
					// not enough bandwidth, return
					return
				}

				glog.Info("bitrate: bandwidth ", ThousandSeparator(int(estimatedBandwidth)), " is enough to send ", ThousandSeparator(int(bc.TotalBitrates()-uint32(additionalBitrate))), " bitrate, increase quality to ", increasedQuality)

				bc.setQuality(claim.track.ID(), increasedQuality)
			}
		}
	}

}

func (bc *bitrateController) calculateDeltaBitrate(fromQuality, toQuality QualityLevel) int32 {
	fromBitrate := bc.client.sfu.QualityLevelToBitrate(fromQuality)
	toBitrate := bc.client.sfu.QualityLevelToBitrate(toQuality)

	return int32(fromBitrate - toBitrate)
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

	simulcastTrack, ok := claim.track.(*SimulcastClientTrack)
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
