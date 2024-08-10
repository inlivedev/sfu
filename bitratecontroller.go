package sfu

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

var (
	ErrAlreadyClaimed          = errors.New("bwcontroller: already claimed")
	ErrorInsufficientBandwidth = errors.New("bwcontroller: bandwidth is insufficient")
)

const (
	DefaultReceiveBitrate = 1_500_000
)

type bitrateClaim struct {
	mu        sync.RWMutex
	track     iClientTrack
	quality   QualityLevel
	simulcast bool
}

func (c *bitrateClaim) Quality() QualityLevel {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.quality
}

func (c *bitrateClaim) SetQuality(quality QualityLevel) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.quality = quality
}

func (c *bitrateClaim) SendBitrate() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.track.SendBitrate()

}

func (c *bitrateClaim) IsAdjustable() bool {
	return c.track.IsSimulcast() || c.track.IsScaleable()
}

func (c *bitrateClaim) QualityLevelToBitrate(quality QualityLevel) uint32 {
	if c.track.IsSimulcast() {
		t := c.track.(*simulcastClientTrack)
		switch quality {
		case QualityLow:
			return t.ReceiveBitrateAtQuality(QualityLow)
		case QualityLowMid:
			return t.ReceiveBitrateAtQuality(QualityLowMid)
		case QualityLowLow:
			return t.ReceiveBitrateAtQuality(QualityLowLow)
		case QualityMid:
			return t.ReceiveBitrateAtQuality(QualityMid)
		case QualityMidMid:
			return t.ReceiveBitrateAtQuality(QualityMidMid)
		case QualityMidLow:
			return t.ReceiveBitrateAtQuality(QualityMidLow)
		case QualityHigh:
			return t.ReceiveBitrateAtQuality(QualityHigh)
		case QualityHighMid:
			return t.ReceiveBitrateAtQuality(QualityHighMid)
		case QualityHighLow:
			return t.ReceiveBitrateAtQuality(QualityHighLow)
		}
	} else {
		switch quality {
		case QualityLowLow:
			return c.track.ReceiveBitrate() / 16
		case QualityLowMid:
			return c.track.ReceiveBitrate() / 8
		case QualityLow:
			return c.track.ReceiveBitrate() / 4
		case QualityMidLow:
			return c.track.ReceiveBitrate() / 8
		case QualityMidMid:
			return c.track.ReceiveBitrate() / 4
		case QualityMid:
			return c.track.ReceiveBitrate() / 2
		case QualityHighLow:
			return c.track.ReceiveBitrate() / 4
		case QualityHighMid:
			return c.track.ReceiveBitrate() / 2
		case QualityHigh:
			return c.track.ReceiveBitrate()
		}
	}

	return 0
}

type bitrateController struct {
	mu                      sync.RWMutex
	lastBitrateAdjustmentTS time.Time
	client                  *Client
	claims                  map[string]*bitrateClaim
	enabledQualityLevels    []QualityLevel
	log                     logging.LeveledLogger
}

func newbitrateController(client *Client, qualityLevels []QualityLevel) *bitrateController {
	bc := &bitrateController{
		mu:                   sync.RWMutex{},
		client:               client,
		claims:               make(map[string]*bitrateClaim, 0),
		enabledQualityLevels: qualityLevels,
		log:                  logging.NewDefaultLoggerFactory().NewLogger("bitratecontroller"),
	}

	go bc.loopMonitor()

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
		total += claim.SendBitrate()
	}

	return total
}

func (bc *bitrateController) setQuality(clientTrackID string, quality QualityLevel) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if claim, ok := bc.claims[clientTrackID]; ok {
		claim.mu.Lock()
		claim.quality = quality
		claim.mu.Unlock()

		bc.claims[clientTrackID] = claim
	}
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

func (bc *bitrateController) addStaticVideoClaims(clientTracks []iClientTrack) (leftTracks []iClientTrack, err error) {
	errors := make([]error, 0)

	leftTracks = make([]iClientTrack, 0)

	for _, clientTrack := range clientTracks {
		if clientTrack.Kind() == webrtc.RTPCodecTypeVideo && !clientTrack.IsSimulcast() && !clientTrack.IsScaleable() {
			_, err := bc.addClaim(clientTrack, QualityHigh)

			if err != nil {
				errors = append(errors, err)
			}
		} else {
			{
				leftTracks = append(leftTracks, clientTrack)
			}
		}
	}

	if len(errors) > 0 {
		return leftTracks, FlattenErrors(errors)
	}

	return leftTracks, nil
}

// calculate the quality level for each track based on the available bandwidth and max bitrate of tracks
func (bc *bitrateController) qualityLevelPerTrack(clientTracks []iClientTrack) QualityLevel {
	maxBitrate := uint32(0)

	for _, clientTrack := range clientTracks {
		if clientTrack.SendBitrate() > maxBitrate {
			maxBitrate = clientTrack.ReceiveBitrate()
		}
	}

	bw := bc.client.GetEstimatedBandwidth()

	received := bc.totalReceivedBitrates()

	var bandwidthLeft uint32

	if bw < received {
		bandwidthLeft = 0
	} else {
		bandwidthLeft = bw - received
	}

	bc.log.Debugf("bitratecontroller: estimated bandwidth %s, received %s, bandwidth left %s", ThousandSeparator(int(bw)), ThousandSeparator(int(received)), ThousandSeparator(int(bandwidthLeft)))

	bandwidthPerTrack := bandwidthLeft / uint32(len(clientTracks))

	bc.log.Debugf("bitratecontroller: bandwidth per track %s", ThousandSeparator(int(bandwidthPerTrack)))

	if bandwidthPerTrack > maxBitrate {
		return QualityHigh
	} else if bandwidthPerTrack > maxBitrate/2 {
		return QualityMid
	} else if bandwidthPerTrack > maxBitrate/4 {
		return QualityLow
	} else if bandwidthPerTrack > maxBitrate/8 {
		return QualityLowMid
	} else {
		return QualityLowLow
	}
}

func (bc *bitrateController) addClaims(clientTracks []iClientTrack) error {
	leftTracks, err := bc.addAudioClaims(clientTracks)
	if err != nil {
		return err
	}

	leftTracks, err = bc.addStaticVideoClaims(leftTracks)
	if err != nil {
		return err
	}

	errors := make([]error, 0)
	claimed := 0

	if len(leftTracks) == 0 {
		return nil
	}

	// calculate the available bandwidth after added all static tracks

	var trackQuality QualityLevel = bc.qualityLevelPerTrack(leftTracks)

	bc.log.Debugf("bitratecontroller: quality level per track is %d for  total %d tracks", trackQuality, len(leftTracks))

	for _, clientTrack := range leftTracks {
		if clientTrack.Kind() == webrtc.RTPCodecTypeVideo {

			// bc.log.Infof("bitratecontroller: track ", clientTrack.ID(), " quality ", trackQuality)
			bc.mu.RLock()
			if _, ok := bc.claims[clientTrack.ID()]; ok {
				errors = append(errors, ErrAlreadyClaimed)
				bc.mu.RUnlock()
				continue
			}
			bc.mu.RUnlock()

			// set last quality that use for requesting PLI after claim added
			if clientTrack.IsSimulcast() {
				clientTrack.(*simulcastClientTrack).lastQuality.Store(uint32(trackQuality))
			} else if clientTrack.IsScaleable() {
				clientTrack.(*scaleableClientTrack).setLastQuality(trackQuality)
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
	bc.mu.Lock()
	defer bc.mu.Unlock()

	bc.claims[clientTrack.ID()] = &bitrateClaim{
		mu:        sync.RWMutex{},
		track:     clientTrack,
		quality:   quality,
		simulcast: clientTrack.IsSimulcast(),
	}

	clientTrack.OnEnded(func() {
		bc.removeClaim(clientTrack.ID())

		clientTrack.Client().stats.removeSenderStats(clientTrack.ID())
	})

	return bc.claims[clientTrack.ID()], nil
}

func (bc *bitrateController) removeClaim(id string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if _, ok := bc.claims[id]; !ok {
		bc.log.Errorf("bitrate: track %s is not exists", id)
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

func (bc *bitrateController) totalSentBitrates() uint32 {
	total := uint32(0)

	for _, claim := range bc.Claims() {
		total += claim.track.SendBitrate()
	}

	return total
}

func (bc *bitrateController) totalReceivedBitrates() uint32 {
	total := uint32(0)

	for _, claim := range bc.Claims() {
		bitrate := claim.track.ReceiveBitrate()
		if bitrate == 0 {
			bitrate = DefaultReceiveBitrate
		}
		total += bitrate
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

func (bc *bitrateController) canIncreaseBitrate(availableBw uint32) bool {
	claims := bc.Claims()

	for _, claim := range claims {
		if claim.IsAdjustable() {
			if claim.Quality() < claim.track.MaxQuality() {
				if bc.isEnoughBandwidthToIncrase(availableBw, claim) {
					return true
				} else {
					bc.log.Tracef("bitratecontroller: can't increase, track %s not enough bandwidth to increase bitrate", claim.track.ID())
				}
			} else {
				bc.log.Tracef("bitratecontroller: can't increase, track %s quality %d is same or higher than max  %d", claim.track.ID(), claim.Quality(), claim.track.MaxQuality())
			}

		}
	}

	return false
}

func (bc *bitrateController) loopMonitor() {
	ctx, cancel := context.WithCancel(bc.client.Context())
	defer cancel()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var needAdjustment bool

			bc.mu.RLock()
			totalSendBitrates := bc.totalSentBitrates()
			bw := bc.client.GetEstimatedBandwidth()
			bc.mu.RUnlock()

			if totalSendBitrates == 0 {
				continue
			}

			bc.log.Debugf("bitratecontroller: available bandwidth %s total bitrate %s", ThousandSeparator(int(bw)), ThousandSeparator(int(totalSendBitrates)))

			var availableBw uint32
			if bw < totalSendBitrates {
				availableBw = 0
			} else {
				availableBw = bw - totalSendBitrates
			}

			if totalSendBitrates < uint32(bw) {
				needAdjustment = bc.canIncreaseBitrate(availableBw)
			} else {
				needAdjustment = bc.canDecreaseBitrate()
			}

			if !needAdjustment {
				continue
			}

			bc.log.Debugf("bitratecontroller: available bandwidth %s total send bitrate %s", ThousandSeparator(int(bw)), ThousandSeparator(int(totalSendBitrates)))

			bc.fitBitratesToBandwidth(uint32(bw))

			bc.mu.Lock()
			bc.lastBitrateAdjustmentTS = time.Now()
			bc.mu.Unlock()

		}
	}

}

// TODO: use video size to prioritize the video. Higher resolution video should have higher priority
func (bc *bitrateController) fitBitratesToBandwidth(bw uint32) {
	totalSentBitrates := bc.totalSentBitrates()

	claims := bc.Claims()
	if totalSentBitrates > bw {
		// reduce bitrates
		for i := QualityHigh; i > QualityLowLow; i-- {
			for _, claim := range claims {
				quality := claim.Quality()
				if claim.IsAdjustable() &&
					quality == QualityLevel(i) {
					oldBitrate := claim.SendBitrate()
					if oldBitrate == 0 {
						continue
					}

					newQuality := bc.getPrevQuality(quality)
					newBitrate := claim.QualityLevelToBitrate(newQuality)
					bitrateGap := oldBitrate - newBitrate
					bc.log.Tracef("bitratecontroller: reduce bitrate for track %s from %d to %d", claim.track.ID(), claim.Quality(), newQuality)
					bc.setQuality(claim.track.ID(), newQuality)

					claim.track.RequestPLI()
					totalSentBitrates = totalSentBitrates - bitrateGap

					bc.log.Infof("bitratecontroller: total sent bitrates %s bandwidth %s", ThousandSeparator(int(totalSentBitrates)), ThousandSeparator(int(bw)))

					// check if the reduced bitrate is fit to the available bandwidth
					if totalSentBitrates <= bw {
						bc.log.Tracef("bitratecontroller: reduce sent bitrates %s to bandwidth %s", ThousandSeparator(int(totalSentBitrates)), ThousandSeparator(int(bw)))
						return
					}
				}
			}
		}
	} else if totalSentBitrates < bw {
		// increase bitrates
		for i := QualityLowLow; i < QualityHigh; i++ {

			for _, claim := range claims {
				quality := claim.Quality()
				if claim.IsAdjustable() &&
					quality == QualityLevel(i) {
					oldBitrate := claim.SendBitrate()

					newQuality := bc.getNextQuality(quality)
					newBitrate := claim.QualityLevelToBitrate(newQuality)
					bitrateIncrease := newBitrate - oldBitrate

					// check if the bitrate increase will more than the available bandwidth
					if totalSentBitrates+bitrateIncrease >= bw {
						bc.log.Tracef("bitratecontroller: increase sent bitrates %s to bandwidth %s", ThousandSeparator(int(totalSentBitrates)), ThousandSeparator(int(bw)))
						return
					}

					bc.log.Tracef("bitratecontroller: increase bitrate for track %s from %d to %d", claim.track.ID(), claim.Quality(), newQuality)
					bc.setQuality(claim.track.ID(), newQuality)
					// update current total bitrates
					totalSentBitrates = totalSentBitrates + bitrateIncrease
					bc.log.Infof("bitratecontroller: total sent bitrates %s bandwidth %s", ThousandSeparator(int(totalSentBitrates)), ThousandSeparator(int(bw)))
					claim.track.RequestPLI()
				}
			}
		}
	}
}

func (bc *bitrateController) getNextQuality(quality QualityLevel) QualityLevel {
	ok := false
	for !ok {
		quality = quality + 1
		if slices.Contains(bc.enabledQualityLevels, quality) {
			ok = true
		}
	}
	return quality
}

func (bc *bitrateController) getPrevQuality(quality QualityLevel) QualityLevel {
	ok := false
	for !ok {
		quality = quality - 1
		if slices.Contains(bc.enabledQualityLevels, quality) {
			ok = true
		}
	}
	return quality
}

func (bc *bitrateController) onRemoteViewedSizeChanged(videoSize videoSize) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	claim, ok := bc.claims[videoSize.TrackID]
	if !ok {
		bc.log.Errorf("bitrate: track %s is not exists", videoSize.TrackID)
		return
	}

	if claim.track.Kind() != webrtc.RTPCodecTypeVideo {
		bc.log.Errorf("bitrate: track %s is not video track", videoSize.TrackID)
		return
	}

	bc.log.Debugf("bitrate: track %s video size changed  %dx%d=%d pixels", videoSize.TrackID, videoSize.Width, videoSize.Height, videoSize.Width*videoSize.Height)

	// TODO: check if it is necessary to set max quality to none
	if videoSize.Width == 0 || videoSize.Height == 0 {
		bc.log.Debugf("bitrate: track  %s video size is 0, set max quality to none", videoSize.TrackID)
		claim.track.SetMaxQuality(QualityNone)
		return
	}

	if videoSize.Width*videoSize.Height < bc.client.sfu.bitrateConfigs.VideoMidPixels {
		bc.log.Debugf("bitrate: track %s video size is low, set max quality to low", videoSize.TrackID)
		claim.track.SetMaxQuality(QualityLow)
	} else if videoSize.Width*videoSize.Height < bc.client.sfu.bitrateConfigs.VideoHighPixels {
		bc.log.Infof("bitrate: track %s video size is mid, set max quality to mid", videoSize.TrackID)
		claim.track.SetMaxQuality(QualityMid)
	} else {
		bc.log.Infof("bitrate: track %s video size is high, set max quality to high", videoSize.TrackID)
		claim.track.SetMaxQuality(QualityHigh)
	}
}

func (bc *bitrateController) isEnoughBandwidthToIncrase(bandwidthLeft uint32, claim *bitrateClaim) bool {
	nextQuality := claim.Quality() + 1

	if nextQuality > QualityHigh {
		return false
	}

	nextBitrate := claim.QualityLevelToBitrate(nextQuality)
	currentBitrate := claim.SendBitrate()

	bandwidthGap := nextBitrate - currentBitrate

	return bandwidthGap < bandwidthLeft
}
