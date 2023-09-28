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

type BitrateClaim struct {
	track   iClientTrack
	bitrate uint32
	quality QualityLevel
	active  bool
}

type BitrateController struct {
	mu     sync.RWMutex
	client *Client
	claims map[string]BitrateClaim
}

func NewBitrateController(client *Client) *BitrateController {
	return &BitrateController{
		mu:     sync.RWMutex{},
		client: client,
		claims: make(map[string]BitrateClaim, 0),
	}
}

func (bc *BitrateController) GetClaims() map[string]BitrateClaim {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	claims := make(map[string]BitrateClaim, 0)
	for k, v := range bc.claims {
		claims[k] = v
	}

	return claims
}

func (bc *BitrateController) Exist(id string) bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; ok {
		return true
	}

	return false
}

func (bc *BitrateController) GetClaim(id string) *BitrateClaim {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if claim, ok := bc.claims[id]; ok {
		return &claim
	}

	return nil
}

// if all is false, only active claims will be counted
func (bc *BitrateController) TotalBitrates(all bool) uint32 {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	return bc.totalBitrates(all)
}

func (bc *BitrateController) totalBitrates(all bool) uint32 {
	total := uint32(0)
	for _, claim := range bc.claims {
		if !all && !claim.active {
			continue
		}

		total += claim.bitrate
	}

	return total
}

func (bc *BitrateController) canIncreaseBitrate(clientTrackID string, quality QualityLevel) bool {
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

func (bc *BitrateController) setQuality(clientTrackID string, quality QualityLevel) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if claim, ok := bc.claims[clientTrackID]; ok {
		bitrate := bc.client.sfu.QualityLevelToBitrate(quality)
		claim.quality = quality
		claim.bitrate = bitrate
		bc.claims[clientTrackID] = claim
	}
}

func (bc *BitrateController) activateClaim(clientTrackID string) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if claim, ok := bc.claims[clientTrackID]; ok {
		claim.active = true
		bc.claims[clientTrackID] = claim
	}
}

// this should be called to check if the quality must be reduced because there is an unactive claim need to fit in the bandwidth
// TODO:
// 1. Need to keep all claims distributed not more than 2 levels.
// 2. If there is a level still in 1, then no level allowed to go up to 3
// 3. The same with reducing level, no level allowed to go down to 1 if there is a level still in 3
func (bc *BitrateController) getNextTrackQuality(clientTrackID string) QualityLevel {
	claim := bc.GetClaim(clientTrackID)
	if claim == nil {
		glog.Warning("client: claim is not exists")
		return QualityNone
	}

	inActiveBitrates := uint32(0)
	highCount := 0
	lowCount := 0

	bc.mu.Lock()

	for _, claim := range bc.claims {
		if !claim.active {
			inActiveBitrates += claim.bitrate
		}

		if claim.track.IsSimulcast() {
			switch claim.quality {
			case QualityHigh:
				highCount++
			case QualityLow:
				lowCount++
			}
		}
	}

	bc.mu.Unlock()

	if inActiveBitrates > 0 && claim.active {
		// need to reduce the quality
		// TODO: check if need to reduce from high to low
		// do we need to set it as inactive?
		if claim.quality > QualityLow {
			nextQuality := claim.quality - 1
			// prevent reduce to low if there is other claims in high
			if nextQuality == QualityLow && highCount > 0 {
				return claim.quality
			}

			bc.setQuality(clientTrackID, nextQuality)

			return nextQuality
		}

		return claim.quality
	}

	// if no need reduction, then check if we need to activate our claim if not yet
	if !claim.active {
		totalActiveBitrates := bc.TotalBitrates(false)
		if totalActiveBitrates+claim.bitrate > bc.client.GetEstimatedBandwidth() {
			// not enough bandwidth to activate the claim, return none because it's not active yet

			return QualityNone
		}

		bc.activateClaim(clientTrackID)
	}

	if claim.quality < QualityHigh {
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

	return claim.quality
}

func (bc *BitrateController) AddClaim(clientTrack iClientTrack, quality QualityLevel) (BitrateClaim, error) {
	bc.mu.Lock()
	if _, ok := bc.claims[clientTrack.ID()]; ok {
		bc.mu.Unlock()
		return BitrateClaim{}, ErrAlreadyClaimed
	}
	bc.mu.Unlock()

	return bc.addClaim(clientTrack, quality, false)

}

func (bc *BitrateController) addClaim(clientTrack iClientTrack, quality QualityLevel, locked bool) (BitrateClaim, error) {
	var currentBitrates uint32

	if locked {
		currentBitrates = bc.totalBitrates(true)
	} else {
		currentBitrates = bc.TotalBitrates(true)
	}

	isActive := false

	var bitrate uint32

	if clientTrack.Kind() == webrtc.RTPCodecTypeAudio {
		bitrate = bc.client.sfu.bitratesConfig.Audio
	} else {
		bitrate = bc.client.sfu.QualityLevelToBitrate(quality)
	}

	// if the new total bitrate is less than the estimated bandwidth, then the track is active
	if currentBitrates+bitrate < bc.client.GetEstimatedBandwidth() {
		isActive = true
	}

	if !locked {
		bc.mu.RLock()
		defer bc.mu.RUnlock()
	}

	bc.claims[clientTrack.ID()] = BitrateClaim{
		track:   clientTrack,
		quality: quality,
		bitrate: bitrate,
		active:  isActive,
	}

	clientTrack.OnTrackEnded(func() {
		bc.RemoveClaim(clientTrack.ID())
	})

	return bc.claims[clientTrack.ID()], nil
}

func (bc *BitrateController) RemoveClaim(id string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; !ok {
		return
	}

	delete(bc.claims, id)
}

func (bc *BitrateController) GetAvailableBandwidth() uint32 {
	return bc.client.GetEstimatedBandwidth() - bc.TotalBitrates(true)
}

func (bc *BitrateController) GetQuality(t *SimulcastClientTrack) QualityLevel {
	track := t.remoteTrack

	var quality QualityLevel

	t.lastCheckQualityTS.Store(time.Now().UnixNano())
	availableBandwidth := bc.GetAvailableBandwidth()

	// need to manually lock to prevent a goroutine add claim while we are checking the claims
	bc.mu.Lock()

	_, exist := bc.claims[t.ID()]
	if !exist {
		// new track

		quality = bc.getDistributedQuality(availableBandwidth)

		claim, err := bc.addClaim(t, quality, true)

		// don't forget to unlock
		bc.mu.Unlock()

		if err != nil && err == ErrAlreadyClaimed {
			if err != nil {
				glog.Error("clienttrack: error on add claim ", err)
			}

			quality = QualityLevel(t.lastQuality.Load())
		} else if !claim.active {
			glog.Warning("clienttrack: claim is not active, claim bitrate ", ThousandSeparator(int(claim.bitrate)), " current bitrate ", ThousandSeparator(int(t.client.bitrateController.TotalBitrates(true))), " available bandwidth ", ThousandSeparator(int(availableBandwidth)))
			quality = QualityNone
		}

	} else {
		// don't forget to unlock
		bc.mu.Unlock()

		// check if the bitrate can be adjusted
		quality = bc.getNextTrackQuality(t.ID())
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

func (bc *BitrateController) getDistributedQuality(availableBandwidth uint32) QualityLevel {
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

	leftBandwidth := availableBandwidth - (uint32(audioTracksCount-audioClaimsCount) * bc.client.sfu.bitratesConfig.Audio) - (uint32(videoTracksCount-videoClaimsCount) * bc.client.sfu.bitratesConfig.Video)

	newTrackCount := uint32(simulcastTracksCount - simulcastClaimsCount)

	if newTrackCount == 0 {
		// all tracks already claimed bitrates, return none
		return QualityNone
	}

	distributedBandwidth := leftBandwidth / newTrackCount

	if distributedBandwidth > bc.client.sfu.bitratesConfig.VideoHigh && highCount > 0 {
		return QualityHigh
	} else if distributedBandwidth < bc.client.sfu.bitratesConfig.VideoHigh && distributedBandwidth > bc.client.sfu.bitratesConfig.VideoMid && (midCount > 0 || highCount > 0) {
		return QualityMid
	} else {
		return QualityLow
	}
}
