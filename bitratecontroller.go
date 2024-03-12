package sfu

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/webrtc/v3"
)

var (
	ErrAlreadyClaimed          = errors.New("bwcontroller: already claimed")
	ErrorInsufficientBandwidth = errors.New("bwcontroller: bandwidth is insufficient")
)

type bitrateClaim struct {
	mu               sync.RWMutex
	track            iClientTrack
	bitrate          uint32
	quality          QualityLevel
	simulcast        bool
	delayCounter     int
	lastIncreaseTime time.Time
	lastDecreaseTime time.Time
}

func (c *bitrateClaim) Quality() QualityLevel {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.quality
}

func (c *bitrateClaim) Bitrate() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.bitrate
}

func (c *bitrateClaim) IsAdjustable() bool {
	return c.track.IsSimulcast() || c.track.IsScaleable()
}

type bitrateController struct {
	mu                      sync.RWMutex
	lastBitrateAdjustmentTS time.Time
	client                  *Client
	claims                  map[string]*bitrateClaim
}

func newbitrateController(client *Client) *bitrateController {
	bc := &bitrateController{
		mu:     sync.RWMutex{},
		client: client,
		claims: make(map[string]*bitrateClaim, 0),
	}

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
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if _, ok := bc.claims[id]; ok {
		return true
	}

	return false
}

func (bc *bitrateController) GetClaim(id string) *bitrateClaim {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if claim, ok := bc.claims[id]; ok && claim != nil {
		return claim
	}

	return nil
}

// if all is false, only active claims will be counted
func (bc *bitrateController) TotalBitrates() uint32 {
	return bc.totalBitrates()
}

func (bc *bitrateController) totalBitrates() uint32 {
	total := uint32(0)
	for _, claim := range bc.Claims() {
		total += claim.bitrate
	}

	return total
}

func (bc *bitrateController) setQuality(clientTrackID string, quality QualityLevel) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if claim, ok := bc.claims[clientTrackID]; ok {
		claim.mu.Lock()

		if claim.quality < quality {
			claim.lastIncreaseTime = time.Now()
		}

		bitrate := bc.client.sfu.QualityLevelToBitrate(quality)
		claim.quality = quality
		claim.bitrate = bitrate
		claim.mu.Unlock()

		bc.claims[clientTrackID] = claim
	}
}

func (bc *bitrateController) setSimulcastClaim(clientTrackID string, simulcast bool) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

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

func (bc *bitrateController) addAudioClaims(clientTracks []iClientTrack) (leftTracks []iClientTrack, err error) {
	errors := make([]error, 0)

	leftTracks = make([]iClientTrack, 0)

	for _, clientTrack := range clientTracks {
		var trackQuality QualityLevel

		if clientTrack.Kind() == webrtc.RTPCodecTypeAudio {
			if clientTrack.LocalTrack().Codec().MimeType == "audio/red" {
				trackQuality = QualityAudioRed
			} else {
				trackQuality = QualityAudio
			}

			_, err := bc.addClaim(clientTrack, trackQuality)

			if err != nil {
				errors = append(errors, err)
			}
		} else {
			leftTracks = append(leftTracks, clientTrack)
		}
	}

	if len(errors) > 0 {
		return leftTracks, FlattenErrors(errors)
	}

	return leftTracks, nil
}

// this should never return QualityNone becaus it will delay onTrack event
func (bc *bitrateController) getDistributedQuality(totalTracks int) QualityLevel {
	if totalTracks == 0 {
		return 0
	}

	availableBandwidth := bc.client.GetEstimatedBandwidth() - bc.totalBitrates()

	distributedBandwidth := availableBandwidth / uint32(totalTracks)

	bitrateConfig := bc.client.SFU().bitrateConfigs

	if distributedBandwidth < bitrateConfig.VideoMid {
		return QualityLow
	} else if distributedBandwidth < bitrateConfig.VideoHigh {
		return QualityMid
	}

	return QualityHigh
}

func (bc *bitrateController) addClaims(clientTracks []iClientTrack) error {
	leftTracks, err := bc.addAudioClaims(clientTracks)
	if err != nil {
		return err
	}

	errors := make([]error, 0)
	claimed := 0

	for _, clientTrack := range leftTracks {
		if clientTrack.Kind() == webrtc.RTPCodecTypeVideo {
			trackQuality := bc.getDistributedQuality(len(leftTracks) - claimed)
			bc.mu.RLock()
			if _, ok := bc.claims[clientTrack.ID()]; ok {
				errors = append(errors, ErrAlreadyClaimed)
				bc.mu.RUnlock()
				continue
			}
			bc.mu.RUnlock()

			if !clientTrack.IsSimulcast() && !clientTrack.IsScaleable() {
				trackQuality = QualityHigh
			}

			// set last quality that use for requesting PLI after claim added
			if clientTrack.IsSimulcast() {
				clientTrack.(*simulcastClientTrack).lastQuality.Store(uint32(trackQuality))
			} else if clientTrack.IsScaleable() {
				clientTrack.(*scaleableClientTrack).lastQuality = trackQuality
			}

			_, err := bc.addClaim(clientTrack, trackQuality)
			if err != nil {
				errors = append(errors, err)
			}
			claimed++
		}
	}

	if len(errors) > 0 {
		return FlattenErrors(errors)
	}

	return nil
}

func (bc *bitrateController) addClaim(clientTrack iClientTrack, quality QualityLevel) (*bitrateClaim, error) {
	bitrate := bc.client.sfu.QualityLevelToBitrate(quality)

	bc.mu.Lock()
	defer bc.mu.Unlock()

	bc.claims[clientTrack.ID()] = &bitrateClaim{
		mu:        sync.RWMutex{},
		track:     clientTrack,
		quality:   quality,
		simulcast: clientTrack.IsSimulcast(),
		bitrate:   bitrate,
	}

	go func() {
		ctx, cancel := context.WithCancel(clientTrack.Context())
		defer cancel()
		<-ctx.Done()
		bc.removeClaim(clientTrack.ID())
		if bc.client.IsDebugEnabled() {
			glog.Info("clienttrack: track ", clientTrack.ID(), " claim removed")
		}
		clientTrack.Client().stats.removeSenderStats(clientTrack.ID())
	}()

	return bc.claims[clientTrack.ID()], nil
}

func (bc *bitrateController) removeClaim(id string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; !ok {
		glog.Error("bitrate: track ", id, " is not exists")
		return
	}

	delete(bc.claims, id)
}

func (bc *bitrateController) exists(id string) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if _, ok := bc.claims[id]; ok {
		return true
	}

	return false
}

func (bc *bitrateController) isScreenNeedIncrease(highestQuality QualityLevel) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	for _, claim := range bc.claims {
		if claim.track.IsScreen() && claim.quality <= highestQuality {
			return true
		}
	}

	return false
}

func (bc *bitrateController) isThereNonScreenCanDecrease(lowestQuality QualityLevel) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	for _, claim := range bc.claims {
		if !claim.track.IsScreen() && (claim.track.IsScaleable() || claim.track.IsSimulcast()) && claim.quality > lowestQuality {
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

	quality := min(claim.quality, t.MaxQuality(), Uint32ToQualityLevel(t.client.quality.Load()))

	if quality != QualityNone && !track.isTrackActive(quality) {
		if quality != QualityLow && track.isTrackActive(QualityLow) {
			return QualityLow
		}

		if quality != QualityMid && track.isTrackActive(QualityMid) {
			return QualityMid
		}

		if quality != QualityHigh && track.isTrackActive(QualityHigh) {
			return QualityHigh
		}
	}

	return quality
}

func (bc *bitrateController) totalSentBitrates() uint32 {
	total := uint32(0)

	for _, claim := range bc.Claims() {
		bc.mu.RLock()
		total += claim.bitrate
		bc.mu.RUnlock()
	}

	return total
}

func (bc *bitrateController) canDecreaseBitrate() bool {
	claims := bc.Claims()

	for _, claim := range claims {
		if claim.IsAdjustable() &&
			claim.Quality() > QualityLow {
			return true
		}
	}

	return false
}

func (bc *bitrateController) needIncreaseBitrate(availableBw uint32) bool {
	claims := bc.Claims()

	for _, claim := range claims {
		if claim.IsAdjustable() &&
			claim.Quality() < claim.track.MaxQuality() &&
			bc.isEnoughBandwidthToIncrase(availableBw, claim) {
			return true
		}
	}

	return false
}

func (bc *bitrateController) MonitorBandwidth(estimator cc.BandwidthEstimator) {
	estimator.OnTargetBitrateChange(func(bw int) {
		var needAdjustment bool

		totalSendBitrates := bc.totalSentBitrates()

		availableBw := uint32(bw) - totalSendBitrates

		if totalSendBitrates < uint32(bw) {
			if bw < int(bc.client.sfu.bitrateConfigs.VideoMid-bc.client.sfu.bitrateConfigs.VideoLow) {
				return
			}

			needAdjustment = bc.needIncreaseBitrate(availableBw)
		} else {
			needAdjustment = bc.canDecreaseBitrate()
		}

		if !needAdjustment {
			return
		}

		glog.Info("bitratecontroller: available bandwidth ", ThousandSeparator(int(bw)), " total bitrate ", ThousandSeparator(int(totalSendBitrates)))

		bc.fitBitratesToBandwidth(uint32(bw))

		bc.mu.Lock()
		bc.lastBitrateAdjustmentTS = time.Now()
		bc.mu.Unlock()

	})
}

func (bc *bitrateController) fitBitratesToBandwidth(bw uint32) {
	totalSentBitrates := bc.totalSentBitrates()

	claims := bc.Claims()
	if totalSentBitrates > bw {
		// reduce bitrates
		for i := QualityHigh; i > QualityLow; i-- {
			for _, claim := range claims {
				if claim.IsAdjustable() &&
					claim.Quality() == QualityLevel(i) {

					glog.Info("bitratecontroller: reduce bitrate for track ", claim.track.ID(), " from ", claim.Quality(), " to ", claim.Quality()-1)
					bc.setQuality(claim.track.ID(), claim.Quality()-1)

					claim.track.RequestPLI()
					totalSentBitrates = bc.totalSentBitrates()

					// check if the reduced bitrate is fit to the available bandwidth
					if totalSentBitrates <= bw {
						glog.Info("bitratecontroller: total sent bitrates ", ThousandSeparator(int(totalSentBitrates)), " available bandwidth ", ThousandSeparator(int(bw)))
						return
					}
				}
			}
		}
	} else {
		// increase bitrates
		for i := QualityLow; i < QualityHigh; i++ {
			for _, claim := range claims {
				if claim.IsAdjustable() &&
					claim.Quality() == QualityLevel(i) {
					oldBitrate := claim.Bitrate()
					newBitrate := bc.client.SFU().QualityLevelToBitrate(claim.Quality() + 1)
					bitrateIncrease := newBitrate - oldBitrate

					// check if the bitrate increase will more than the available bandwidth
					if totalSentBitrates+bitrateIncrease >= bw {
						return
					}

					glog.Info("bitratecontroller: increase bitrate for track ", claim.track.ID(), " from ", claim.Quality(), " to ", claim.Quality()+1)
					bc.setQuality(claim.track.ID(), claim.Quality()+1)
					// update current total bitrates
					totalSentBitrates = bc.totalSentBitrates()
					claim.track.RequestPLI()
				}
			}
		}
	}
}

func (bc *bitrateController) onRemoteViewedSizeChanged(videoSize videoSize) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	claim, ok := bc.claims[videoSize.TrackID]
	if !ok {
		glog.Error("bitrate: track ", videoSize.TrackID, " is not exists")
		return
	}

	if claim.track.Kind() != webrtc.RTPCodecTypeVideo {
		glog.Error("bitrate: track ", videoSize.TrackID, " is not video track")
		return
	}

	glog.Info("bitrate: track ", videoSize.TrackID, " video size changed ", videoSize.Width, "x", videoSize.Height, "=", videoSize.Width*videoSize.Height, " pixels")

	if videoSize.Width == 0 || videoSize.Height == 0 {
		glog.Info("bitrate: track ", videoSize.TrackID, " video size is 0, set max quality to none")
		claim.track.SetMaxQuality(QualityNone)
	}

	if videoSize.Width*videoSize.Height <= bc.client.sfu.bitrateConfigs.VideoLowPixels {
		glog.Info("bitrate: track ", videoSize.TrackID, " video size is low, set max quality to low")
		claim.track.SetMaxQuality(QualityLow)
	} else if videoSize.Width*videoSize.Height <= bc.client.sfu.bitrateConfigs.VideoMidPixels {
		glog.Info("bitrate: track ", videoSize.TrackID, " video size is mid, set max quality to mid")
		claim.track.SetMaxQuality(QualityMid)
	} else {
		glog.Info("bitrate: track ", videoSize.TrackID, " video size is high, set max quality to high")
		claim.track.SetMaxQuality(QualityHigh)
	}
}

func (bc *bitrateController) isEnoughBandwidthToIncrase(bandwidthLeft uint32, claim *bitrateClaim) bool {
	nextQuality := claim.Quality() + 1

	if nextQuality > QualityHigh {
		return false
	}

	nextBitrate := bc.client.sfu.QualityLevelToBitrate(nextQuality)
	currentBitrate := bc.client.sfu.QualityLevelToBitrate(claim.Quality())

	bandwidthGap := nextBitrate - currentBitrate

	return bandwidthGap < bandwidthLeft
}
