package sfu

import (
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
	active    bool
	simulcast bool
}

type bitrateController struct {
	mu     sync.RWMutex
	client *Client
	claims map[string]bitrateClaim
}

func newbitrateController(client *Client) *bitrateController {
	return &bitrateController{
		mu:     sync.RWMutex{},
		client: client,
		claims: make(map[string]bitrateClaim, 0),
	}
}

func (bc *bitrateController) GetClaims() map[string]bitrateClaim {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	claims := make(map[string]bitrateClaim, 0)
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
		return &claim
	}

	return nil
}

// if all is false, only active claims will be counted
func (bc *bitrateController) TotalBitrates(all bool) uint32 {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	return bc.totalBitrates(all)
}

func (bc *bitrateController) totalBitrates(all bool) uint32 {
	total := uint32(0)
	for _, claim := range bc.claims {
		if !all && !claim.active {
			continue
		}

		total += claim.bitrate
	}

	return total
}

func (bc *bitrateController) canIncreaseBitrate(clientTrackID string, quality QualityLevel) bool {
	delta := uint32(0)

	newBitrate := bc.client.sfu.QualityLevelToBitrate(quality)

	bc.mu.Lock()
	claim, ok := bc.claims[clientTrackID]
	bc.mu.Unlock()

	if !ok {
		return false
	}

	delta = newBitrate - claim.bitrate

	newEstimatedBandwidth := bc.TotalBitrates(true) + delta
	bandwidth := bc.client.GetEstimatedBandwidth()

	return newEstimatedBandwidth < bandwidth
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

func (bc *bitrateController) setClaimActive(clientTrackID string, active bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if claim, ok := bc.claims[clientTrackID]; ok {
		claim.active = active
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
			if !claim.active {
				bc.setClaimActive(claim.track.ID(), true)
			}

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

// this should be called to check if the quality must be reduced because there is an unactive claim need to fit in the bandwidth
// TODO:
// 1. Need to keep all claims distributed not more than 2 levels.
// 2. If there is a level still in 1, then no level allowed to go up to 3
// 3. The same with reducing level, no level allowed to go down to 1 if there is a level still in 3
func (bc *bitrateController) getNextTrackQuality(clientTrackID string, availableBandwidth int32) QualityLevel {
	claim := bc.GetClaim(clientTrackID)
	if claim == nil {
		glog.Warning("client: claim is not exists")
		return QualityNone
	}

	// check if the simulcast tracks all available
	// this to prevent we switch quality on RID track but the other RID track is not available
	// because the browser doesn't support simulcast with the codec
	if ok, quality := bc.checkAllTrackActive(claim); !ok {
		return quality
	}

	unActiveBitrates := uint32(0)
	highCount := 0
	midCount := 0
	lowCount := 0

	bc.mu.Lock()

	for _, claim := range bc.claims {
		if !claim.active {
			unActiveBitrates += claim.bitrate
		}

		if claim.simulcast && claim.active && claim.track.Kind() == webrtc.RTPCodecTypeVideo {
			switch claim.quality {
			case QualityHigh:
				highCount++
			case QualityMid:
				midCount++
			case QualityLow:
				lowCount++
			}
		}
	}

	bc.mu.Unlock()

	if (unActiveBitrates > 0 && claim.active) ||
		(availableBandwidth < 0 && claim.active) ||
		(claim.active && claim.quality == QualityHigh && lowCount > 0) {
		// need to reduce the quality if there is an unactive claim and quality distribution is not balanced
		// TODO: check if need to reduce from high to low
		nextQuality := claim.quality - 1

		// prevent reduce to low if there it make unbalanced quality distribution
		if (nextQuality == QualityLow && highCount > 0) || (nextQuality == QualityNone && midCount > 0) {
			return claim.quality
		}

		// no reduction to none or unactivate for screen
		if nextQuality == QualityNone && claim.track.IsScreen() {
			return claim.quality
		}

		if nextQuality == QualityNone {
			// unactivate the claim instead of reducing to none to make sure we still have claimed bitrate
			bc.setClaimActive(clientTrackID, false)
			return nextQuality
		}

		bc.setQuality(clientTrackID, nextQuality)

		return nextQuality

	}

	// if no need reduction, then check if we need to activate our claim if not yet
	if !claim.active {
		totalActiveBitrates := bc.TotalBitrates(false)
		if totalActiveBitrates+claim.bitrate > bc.client.GetEstimatedBandwidth() {
			// not enough bandwidth to activate the claim, return none because it's not active yet

			return QualityNone
		}

		bc.setClaimActive(clientTrackID, true)
	}

	if claim.quality < QualityHigh && claim.active {
		nextQuality := claim.quality + 1

		if bc.canIncreaseBitrate(clientTrackID, nextQuality) {
			// prevent increase to high if there is other claims in low
			if nextQuality == QualityHigh && lowCount > 0 {
				return claim.quality
			}

			bc.setQuality(clientTrackID, nextQuality)

			return nextQuality
		}
	}

	if !claim.active {
		return QualityNone
	}

	return claim.quality
}

func (bc *bitrateController) AddClaims(clientTracks []iClientTrack) error {
	availableBandwidth := bc.availableBandwidth()
	quality := bc.getDistributedQuality(availableBandwidth)

	bc.mu.Lock()
	defer bc.mu.Unlock()

	errors := make([]error, 0)

	for _, clientTrack := range clientTracks {
		if _, ok := bc.claims[clientTrack.ID()]; ok {
			errors = append(errors, ErrAlreadyClaimed)
			continue
		}

		_, err := bc.addClaim(clientTrack, quality, true)
		if err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		return FlattenErrors(errors)
	}

	return nil
}

func (bc *bitrateController) AddClaim(clientTrack iClientTrack, quality QualityLevel) (bitrateClaim, error) {
	bc.mu.Lock()
	if _, ok := bc.claims[clientTrack.ID()]; ok {
		bc.mu.Unlock()
		return bitrateClaim{}, ErrAlreadyClaimed
	}
	bc.mu.Unlock()

	return bc.addClaim(clientTrack, quality, false)

}

func (bc *bitrateController) addClaim(clientTrack iClientTrack, quality QualityLevel, locked bool) (bitrateClaim, error) {
	var bitrate uint32

	if clientTrack.Kind() == webrtc.RTPCodecTypeAudio {
		if clientTrack.LocalTrack().Codec().MimeType == "audio/RED" {
			bitrate = bc.client.sfu.bitratesConfig.AudioRed
		} else {
			bitrate = bc.client.sfu.bitratesConfig.Audio
		}
	} else {
		bitrate = bc.client.sfu.QualityLevelToBitrate(quality)
	}

	if !locked {
		bc.mu.RLock()
		defer bc.mu.RUnlock()
	}

	bc.claims[clientTrack.ID()] = bitrateClaim{
		track:     clientTrack,
		quality:   quality,
		simulcast: clientTrack.IsSimulcast(),
		bitrate:   bitrate,
		active:    true,
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

// getAvailableBandwidth returns the available bandwidth for the client
// The available bandwidth is the estimated bandwidth minus the total bitrate of all active claims
// Use this information for debugging or logging purpose
func (bc *bitrateController) availableBandwidth() int32 {
	return int32(bc.client.GetEstimatedBandwidth()) - int32(bc.TotalBitrates(true))
}

func (bc *bitrateController) GetQuality(t *SimulcastClientTrack) QualityLevel {
	track := t.remoteTrack

	var quality QualityLevel

	t.lastCheckQualityTS.Store(time.Now().UnixNano())
	availableBandwidth := bc.availableBandwidth()

	// need to manually lock to prevent a goroutine add claim while we are checking the claims
	bc.mu.Lock()

	_, exist := bc.claims[t.ID()]
	if !exist {
		// new track

		isActive := false

		if availableBandwidth > 0 {
			isActive = true
			quality = bc.getDistributedQuality(availableBandwidth)
		}

		claim, err := bc.addClaim(t, quality, isActive)
		glog.Info("bcontroller: add new claim for track ", t.ID(), " quality ", quality, " bitrate ", ThousandSeparator(int(claim.bitrate)), " available bandwidth ", ThousandSeparator(int(availableBandwidth)))
		// don't forget to unlock
		bc.mu.Unlock()

		if err != nil && err == ErrAlreadyClaimed {
			if err != nil {
				glog.Error("clienttrack: error on add claim ", err)
			}

			quality = QualityLevel(t.lastQuality.Load())
		} else if !claim.active {
			glog.Warning("clienttrack: claim is not active, claim bitrate ", ThousandSeparator(int(claim.bitrate)), " current active bitrate ", ThousandSeparator(int(t.client.bitrateController.TotalBitrates(false))), " available bandwidth ", ThousandSeparator(int(availableBandwidth)))
			quality = QualityNone
		}

	} else {
		// don't forget to unlock
		bc.mu.Unlock()

		// check if the bitrate can be adjusted
		quality = bc.getNextTrackQuality(t.ID(), availableBandwidth)
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
			glog.Warning("bitrate: quality ", quality, " and fallback quality ", lastQuality, " is not active, so sending no qualty. Available bandwidth: ", ThousandSeparator(int(availableBandwidth)), "active bitrate: ", ThousandSeparator(int(t.client.bitrateController.TotalBitrates(false))))

			return QualityNone
		}

		glog.Warning("bitrate: quality ", quality, " is not active,  fallback to last quality ", lastQuality, ". Available bandwidth: ", ThousandSeparator(int(availableBandwidth)), "active bitrate: ", ThousandSeparator(int(t.client.bitrateController.TotalBitrates(false))))

		return lastQuality
	}

	return quality
}

func (bc *bitrateController) getDistributedQuality(availableBandwidth int32) QualityLevel {
	audioTracksCount := 0
	videoTracksCount := 0
	simulcastTracksCount := 0

	for _, track := range bc.client.publishedTracks.GetTracks() {
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

	leftBandwidth := availableBandwidth - (int32(audioTracksCount-audioClaimsCount) * int32(bc.client.sfu.bitratesConfig.Audio)) - (int32(videoTracksCount-videoClaimsCount) * int32(bc.client.sfu.bitratesConfig.Video))

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
