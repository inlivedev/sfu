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
	client               *Client
	claims               sync.Map
	enabledQualityLevels []QualityLevel
	log                  logging.LeveledLogger
}

func newbitrateController(client *Client, qualityLevels []QualityLevel) *bitrateController {
	bc := &bitrateController{
		client:               client,
		claims:               sync.Map{},
		enabledQualityLevels: qualityLevels,
		log:                  logging.NewDefaultLoggerFactory().NewLogger("bitratecontroller"),
	}

	go bc.loopMonitor()

	return bc
}

func (bc *bitrateController) Claims() map[string]*bitrateClaim {
	claims := make(map[string]*bitrateClaim, 0)
	bc.claims.Range(func(key, value interface{}) bool {
		claims[key.(string)] = value.(*bitrateClaim)
		return true
	})

	return claims
}

func (bc *bitrateController) Exist(id string) bool {
	if _, ok := bc.claims.Load(id); ok {
		return true
	}

	return false
}

func (bc *bitrateController) GetClaim(id string) *bitrateClaim {
	claim, ok := bc.claims.Load(id)
	if ok && claim != nil {
		return claim.(*bitrateClaim)
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
	if val, ok := bc.claims.Load(clientTrackID); ok {
		claim := val.(*bitrateClaim)
		claim.SetQuality(quality)

		bc.claims.Store(clientTrackID, claim)
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

			if _, ok := bc.claims.Load(clientTrack.ID()); ok {
				errors = append(errors, ErrAlreadyClaimed)
				continue
			}

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
	claim := &bitrateClaim{
		mu:        sync.RWMutex{},
		track:     clientTrack,
		quality:   quality,
		simulcast: clientTrack.IsSimulcast(),
	}

	bc.claims.Store(clientTrack.ID(), claim)

	clientTrack.OnEnded(func() {
		bc.removeClaim(clientTrack.ID())

		clientTrack.Client().stats.removeSenderStats(clientTrack.ID())
	})

	return claim, nil
}

func (bc *bitrateController) removeClaim(id string) {
	if _, exist := bc.claims.LoadAndDelete(id); !exist {
		bc.log.Errorf("bitrate: track %s is not exists", id)
	}
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
				return bc.isEnoughBandwidthToIncrase(availableBw, claim)
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

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var needAdjustment bool

			totalSendBitrates := bc.totalSentBitrates()
			bw := bc.client.GetEstimatedBandwidth()

			if totalSendBitrates == 0 {
				bc.log.Trace("bitratecontroller: no track to adjust")
				continue
			}

			var availableBw uint32
			if bw < totalSendBitrates {
				availableBw = 0
			} else {
				availableBw = bw - totalSendBitrates
			}

			if totalSendBitrates < uint32(bw) {
				needAdjustment = bc.canIncreaseBitrate(availableBw)
				if needAdjustment {
					bc.log.Tracef("bitratecontroller: need to increase bitrate, available bandwidth %s", ThousandSeparator(int(availableBw)))
				}
			} else {
				needAdjustment = bc.canDecreaseBitrate()
				if needAdjustment {
					bc.log.Tracef("bitratecontroller: need to decrease bitrate, available bandwidth ", ThousandSeparator(int(availableBw)))
				}
			}

			if !needAdjustment {
				continue
			}

			bc.fitBitratesToBandwidth(uint32(bw))
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
			bc.log.Trace("bitratecontroller: trying to reduce bitrate")
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
		bc.log.Trace("bitratecontroller: trying to increase bitrate")
		// increase bitrates
		for i := QualityLowLow; i < QualityHigh; i++ {
			for _, claim := range claims {
				quality := claim.Quality()
				if claim.IsAdjustable() &&
					quality == QualityLevel(i) &&
					quality < claim.track.MaxQuality() {
					oldBitrate := claim.SendBitrate()

					newQuality := bc.getNextQuality(quality)
					newBitrate := claim.QualityLevelToBitrate(newQuality)
					bitrateIncrease := newBitrate - oldBitrate

					// check if the bitrate increase will more than the available bandwidth
					newSentBitrates := totalSentBitrates + bitrateIncrease
					if newSentBitrates > bw {
						bc.log.Tracef("bitratecontroller: can't increase, new bitrates %s not fit to bandwidth %s", ThousandSeparator(int(newSentBitrates)), ThousandSeparator(int(bw)))
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
	val, ok := bc.claims.Load(videoSize.TrackID)
	if !ok {
		bc.log.Errorf("bitrate: track %s is not exists", videoSize.TrackID)
		return
	}

	claim := val.(*bitrateClaim)

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

	if videoSize.Width*videoSize.Height < bc.client.sfu.bitrateConfigs.VideoLowPixels {
		bc.log.Debugf("bitrate: track %s video size is low, set max quality to low", videoSize.TrackID)
		claim.track.SetMaxQuality(QualityLow)
	} else if videoSize.Width*videoSize.Height < bc.client.sfu.bitrateConfigs.VideoMidPixels {
		bc.log.Infof("bitrate: track %s video size is mid, set max quality to mid", videoSize.TrackID)
		claim.track.SetMaxQuality(QualityMid)
	} else {
		bc.log.Infof("bitrate: track %s video size is high, set max quality to high", videoSize.TrackID)
		claim.track.SetMaxQuality(QualityHigh)
	}
}

func (bc *bitrateController) isEnoughBandwidthToIncrase(bandwidthLeft uint32, claim *bitrateClaim) bool {
	nextQuality := bc.getNextQuality(claim.Quality())

	if nextQuality > QualityHigh {
		bc.log.Tracef("bitratecontroller: won't increase track %s quality %d is already high", claim.track.ID(), nextQuality)
		return false
	}

	nextBitrate := claim.QualityLevelToBitrate(nextQuality)
	currentBitrate := claim.SendBitrate()

	if nextBitrate <= currentBitrate {
		bc.log.Tracef("bitratecontroller: can't increase the bitrate, next bitrate %d is less than current %d", nextBitrate, currentBitrate)
		return false
	}

	bandwidthGap := nextBitrate - currentBitrate

	if bandwidthGap < bandwidthLeft {
		bc.log.Tracef("bitratecontroller: track %s can increase, next bandwidth %d, bandwidth left %d", claim.track.ID(), bandwidthGap, bandwidthLeft)
		return true
	}

	bc.log.Tracef("bitratecontroller: track %s can't increase, need bandwidth %d to increase, but bandwidth left %d", claim.track.ID(), bandwidthGap, bandwidthLeft)
	return false
}
