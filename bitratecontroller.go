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
	decreaseBitrate = -1
)

type bitrateAdjustment int

type bitrateClaim struct {
	mu               sync.Mutex
	track            iClientTrack
	bitrate          uint32
	quality          QualityLevel
	simulcast        bool
	delayCounter     int
	lastIncreaseTime time.Time
	lastDecreaseTime time.Time
}

func (c *bitrateClaim) Quality() QualityLevel {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.quality
}

func (c *bitrateClaim) isAllowToIncrease() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.delayCounter > 0 && time.Since(c.lastIncreaseTime) < time.Duration(c.delayCounter)*10*time.Second {
		glog.Info("clienttrack: delay increase,  delay counter ", c.delayCounter)

		return false
	}

	return true
}

func (c *bitrateClaim) pushbackDelayCounter() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.delayCounter == 0 {
		c.delayCounter = 1
		return
	}

	c.delayCounter = int(math.Ceil(float64(c.delayCounter) * 1.5))
	glog.Info("clienttrack: pushback delay counter to ", c.delayCounter)
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

	if claim, ok := bc.claims[id]; ok && claim != nil {
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

func (bc *bitrateController) addAudioClaims(clientTracks []iClientTrack) (leftTracks []iClientTrack, err error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	errors := make([]error, 0)

	leftTracks = make([]iClientTrack, 0)

	for _, clientTrack := range clientTracks {
		var trackQuality QualityLevel

		if clientTrack.Kind() == webrtc.RTPCodecTypeAudio {
			if clientTrack.LocalTrack().Codec().MimeType == "audio/RED" {
				trackQuality = QualityAudioRed
			} else {
				trackQuality = QualityAudio
			}

			_, err := bc.addClaim(clientTrack, trackQuality, true)

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

func (bc *bitrateController) getDistributedQuality(totalTracks int) QualityLevel {
	if totalTracks == 0 {
		return 0
	}

	availableBandwidth := bc.client.GetEstimatedBandwidth() - bc.totalBitrates()

	distributedBandwidth := availableBandwidth / uint32(totalTracks)

	bitrateConfig := bc.client.SFU().bitratesConfig

	if distributedBandwidth < bitrateConfig.VideoLow {
		return QualityNone
	} else if distributedBandwidth < bitrateConfig.VideoMid {
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

	bc.mu.Lock()
	defer bc.mu.Unlock()

	errors := make([]error, 0)
	claimed := 0

	for _, clientTrack := range leftTracks {
		if clientTrack.Kind() == webrtc.RTPCodecTypeVideo {
			trackQuality := bc.getDistributedQuality(len(leftTracks) - claimed)
			if clientTrack.IsScreen() && trackQuality == QualityNone {
				trackQuality = QualityLow
			}

			if _, ok := bc.claims[clientTrack.ID()]; ok {
				errors = append(errors, ErrAlreadyClaimed)
				continue
			}

			if !clientTrack.IsSimulcast() && !clientTrack.IsScaleable() {
				trackQuality = QualityHigh
			}

			// set last quality that use for requesting PLI after claim added
			if clientTrack.IsSimulcast() {
				clientTrack.(*simulcastClientTrack).lastQuality.Store(uint32(trackQuality))
			} else if clientTrack.IsScaleable() {
				clientTrack.(*scaleableClientTrack).lastQuality = trackQuality
			}

			_, err := bc.addClaim(clientTrack, trackQuality, true)
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

func (bc *bitrateController) addClaim(clientTrack iClientTrack, quality QualityLevel, locked bool) (*bitrateClaim, error) {
	bitrate := bc.client.sfu.QualityLevelToBitrate(quality)

	if !locked {
		bc.mu.RLock()
		defer bc.mu.RUnlock()
	}

	bc.claims[clientTrack.ID()] = &bitrateClaim{
		mu:        sync.Mutex{},
		track:     clientTrack,
		quality:   quality,
		simulcast: clientTrack.IsSimulcast(),
		bitrate:   bitrate,
	}

	clientTrack.OnTrackEnded(func() {
		bc.removeClaim(clientTrack.ID())
		clientTrack.Client().stats.removeSenderStats(clientTrack.ID())
	})

	return bc.claims[clientTrack.ID()], nil
}

func (bc *bitrateController) removeClaim(id string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; !ok {
		return
	}

	delete(bc.claims, id)
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

	quality := min(claim.quality, t.MaxQuality(), Uint32ToQualityLevel(t.client.quality.Load()))

	if quality != QualityNone && !track.isTrackActive(quality) {
		if quality != QualityLow && track.isTrackActive(QualityLow) {
			return QualityLow
		}

		lastQuality := QualityLevel(t.lastQuality.Load())
		if track.isTrackActive(lastQuality) {
			return lastQuality
		}

		return QualityNone
	}

	return quality
}

func (bc *bitrateController) totalSentBitrates() uint32 {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	total := uint32(0)

	for _, claim := range bc.claims {
		total += claim.bitrate
	}

	return total
}

func (bc *bitrateController) start() {
	go func() {
		context, cancel := context.WithCancel(bc.client.context)
		defer cancel()

		ticker := time.NewTicker(3 * time.Second)
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
// each time adjustment needed, it will only increase or decrese single track.
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

	for _, claim := range claims {
		if claim.track.IsSimulcast() || claim.track.IsScaleable() {
			maxQuality := claim.track.MaxQuality()
			if claim.quality > claim.track.MaxQuality() {
				bc.setQuality(claim.track.ID(), maxQuality)
			}

			bitrateAdjustment := bc.getBitrateAdjustment(claim)

			if bitrateAdjustment == keepBitrate {
				if (claim.track.IsSimulcast() && claim.track.(*simulcastClientTrack).remoteTrack.isTrackActive(claim.quality)) || claim.track.IsScaleable() {
					continue
				}

				bitrateAdjustment = decreaseBitrate
			}

			if bitrateAdjustment == decreaseBitrate {
				if (claim.track.IsSimulcast() || claim.track.IsScaleable()) && claim.quality > QualityLow {
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
					if claim.track.IsSimulcast() {
						claim.track.(*simulcastClientTrack).remoteTrack.sendPLI(reducedQuality)
					} else {
						claim.track.RequestPLI()
					}

					glog.Info("clienttrack: send pli for track ", claim.track.ID(), " quality ", reducedQuality, " changed from ", claim.quality)
					bc.setQuality(claim.track.ID(), reducedQuality)

					return
				}

			} else if bitrateAdjustment == increaseBitrate {
				if (claim.track.IsSimulcast() || claim.track.IsScaleable()) && claim.quality < QualityHigh {
					increasedQuality := claim.quality + 1

					if claim.quality == QualityMid && noneCount+lowCount > 0 {
						continue
					} else if claim.quality == QualityLow && noneCount > 0 {
						continue
					}

					if !claim.track.IsScreen() && bc.isScreenNeedIncrease(highestQuality) {
						continue
					}

					if claim.track.IsSimulcast() {
						claim.track.(*simulcastClientTrack).remoteTrack.sendPLI(increasedQuality)
					} else {
						claim.track.RequestPLI()
					}

					if bc.client.IsDebugEnabled() {
						glog.Info("clienttrack: send pli for track ", claim.track.ID(), " quality ", increasedQuality, " changed from ", claim.quality)
					}

					// don't increase if the quality is higher than allowed max quality
					if increasedQuality > claim.track.MaxQuality() {
						continue
					}

					bc.setQuality(claim.track.ID(), increasedQuality)

					return
				}

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

	if videoSize.Width == 0 || videoSize.Height == 0 {
		claim.track.SetMaxQuality(QualityNone)
	}

	if videoSize.Width <= bc.client.sfu.bitratesConfig.VideoLowPixels {
		claim.track.SetMaxQuality(QualityLow)
	} else if videoSize.Width <= bc.client.sfu.bitratesConfig.VideoMidPixels {
		claim.track.SetMaxQuality(QualityMid)
	} else {
		claim.track.SetMaxQuality(QualityHigh)
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
//
// TODO:
// - need to check if the track is keep increase but then decrease again
// - if it happen twice, then we need to delay when to increase the bitrate
// - by adding keepBitrate delay counter
// - each time the bitrate increase it will check the delay counter if not 0 then no increase but decrease the counter
// - if the counter is 0 then increase the bitrate
// - if the bitrate back to decrease then the delay counter will add 1.5x of the previous delay counter
func (bc *bitrateController) getBitrateAdjustment(claim *bitrateClaim) bitrateAdjustment {
	// don't adjust bitrates too fast
	if time.Since(claim.lastDecreaseTime) < 2*time.Second || time.Since(claim.lastIncreaseTime) < 2*time.Second {
		return keepBitrate
	}

	if claim.track.IsSimulcast() {
		track := claim.track.(*simulcastClientTrack)
		switch claim.quality {
		case QualityHigh:
			if track.remoteTrack.remoteTrackHigh == nil {
				return decreaseBitrate
			}
		case QualityMid:
			if track.remoteTrack.remoteTrackMid == nil {
				return decreaseBitrate
			}
		case QualityLow:
			if track.remoteTrack.remoteTrackLow == nil {
				return increaseBitrate
			}
		}
	}

	availableBandwidth := bc.client.GetEstimatedBandwidth()

	if bc.client.estimator != nil {
		return bc.getBitrateBasedAdjustment(availableBandwidth, claim)
	}

	return bc.getLossBasedAdjustment(claim)
}

func (bc *bitrateController) getBitrateBasedAdjustment(bandwidth uint32, claim *bitrateClaim) bitrateAdjustment {
	if claim.track.Kind() == webrtc.RTPCodecTypeAudio {
		return keepBitrate
	}

	totalBitrates := bc.totalSentBitrates()
	if totalBitrates > bandwidth && claim.quality != QualityNone {
		// if we got decrease after we increase within short time, then we need to delay the next increase
		if time.Since(claim.lastIncreaseTime) < 10*time.Second {
			if bc.client.IsDebugEnabled() {
				glog.Info("bitrate: track ", claim.track.ID(), " decrease bitrate too fast, delay increase bitrate")
			}
			claim.pushbackDelayCounter()
		}

		if bc.client.IsDebugEnabled() {
			glog.Info("bitrate: track ", claim.track.ID(), " decrease bitrate. Availabel bandwidth ", ThousandSeparator(int(bandwidth)), " total bitrate ", ThousandSeparator(int(totalBitrates)))
		}

		return decreaseBitrate
	} else if totalBitrates < bandwidth && claim.quality != QualityHigh {
		if !claim.isAllowToIncrease() {
			if bc.client.IsDebugEnabled() {
				glog.Info("bitrate: track ", claim.track.ID(), " increase bitrate too fast, delay increase bitrate")
			}
			return keepBitrate
		}

		if !bc.isEnoughBandwidthToIncrase(bandwidth, claim) {
			if bc.client.IsDebugEnabled() {
				glog.Info("bitrate: track ", claim.track.ID(), " not enough bandwidth to increase bitrate")
			}

			return keepBitrate
		}

		if bc.client.IsDebugEnabled() {
			glog.Info("bitrate: track ", claim.track.ID(), " increase bitrate. Availabel bandwidth ", ThousandSeparator(int(bandwidth)), " total bitrate ", ThousandSeparator(int(totalBitrates)))
		}

		return increaseBitrate
	}

	return keepBitrate
}

func (bc *bitrateController) isEnoughBandwidthToIncrase(bandwidthLeft uint32, claim *bitrateClaim) bool {
	nextQuality := claim.quality + 1

	if nextQuality > QualityHigh {
		return false
	}

	nextBitrate := bc.client.sfu.QualityLevelToBitrate(nextQuality)
	currentBitrate := bc.client.sfu.QualityLevelToBitrate(claim.quality)

	bandwidthGap := nextBitrate - currentBitrate

	return bandwidthGap < bandwidthLeft
}

func (bc *bitrateController) getLossBasedAdjustment(claim *bitrateClaim) bitrateAdjustment {
	sender, err := bc.client.stats.GetSender(claim.track.ID())
	if err != nil {
		glog.Error("bitrate: track ", claim.track.ID(), " is not exists")
		return keepBitrate
	}

	lostSentRatio := sender.RemoteInboundRTPStreamStats.FractionLost

	if lostSentRatio < 0.02 && claim.quality != QualityHigh {
		if bc.client.IsDebugEnabled() {
			glog.Info("bitrate: track ", claim.track.ID(), " lost ratio ", lostSentRatio, " can increase bitrate")
		}

		if !claim.isAllowToIncrease() {
			if bc.client.IsDebugEnabled() {
				glog.Info("bitrate: track ", claim.track.ID(), " increase bitrate too fast, delay increase bitrate")
			}
			return keepBitrate
		}

		return increaseBitrate
	} else if lostSentRatio > 0.1 && claim.quality != QualityNone {
		if bc.client.IsDebugEnabled() {
			glog.Info("bitrate: track ", claim.track.ID(), " lost ratio ", lostSentRatio, " need to decrease bitrate")
		}

		if bc.client.IsDebugEnabled() {
			glog.Info("last increase time ", time.Since(claim.lastIncreaseTime).Milliseconds(), " ms")
		}

		// if we got decrease after we increase within short time, then we need to delay the next increase
		if time.Since(claim.lastIncreaseTime) < 10*time.Second {
			if bc.client.IsDebugEnabled() {
				glog.Info("bitrate: track ", claim.track.ID(), " decrease bitrate too fast, delay increase bitrate")
			}
			claim.pushbackDelayCounter()
		}

		return decreaseBitrate
	}

	return keepBitrate
}
