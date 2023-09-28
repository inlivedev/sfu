package sfu

import (
	"errors"
	"sync"

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

// will return zero if there is no inactive claimed bitrate
func (bc *BitrateController) inactiveClaimBitrates() uint32 {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	total := uint32(0)

	for _, claim := range bc.claims {
		if !claim.active {
			total += claim.bitrate
		}
	}

	return total
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
func (bc *BitrateController) getNextTrackQuality(clientTrackID string) QualityLevel {
	claim := bc.GetClaim(clientTrackID)
	if claim == nil {
		glog.Warning("client: claim is not exists")
		return QualityNone
	}

	needBitrates := bc.inactiveClaimBitrates()

	if needBitrates > 0 && claim.active {
		// need to reduce the quality
		// TODO: check if need to reduce from high to low
		// do we need to set it as inactive?
		if claim.quality > QualityLow {
			nextQuality := claim.quality - 1
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

	currentBitrates := bc.TotalBitrates(true)
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

	bc.mu.RLock()
	defer bc.mu.RUnlock()

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
