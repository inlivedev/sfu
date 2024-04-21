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
		case QualityMid:
			return t.ReceiveBitrateAtQuality(QualityMid)
		case QualityHigh:
			return t.ReceiveBitrateAtQuality(QualityHigh)
		}
	} else {
		switch quality {
		case QualityLow:
			return c.track.ReceiveBitrate() / 4
		case QualityMid:
			return c.track.ReceiveBitrate() / 2
		case QualityHigh:
			return c.track.ReceiveBitrate()
		}
	}

	return 0
}

type bitrateController struct {
	mu                      sync.RWMutex
	targetBitrate           uint32
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

func (bc *bitrateController) addClaims(clientTracks []iClientTrack) error {
	leftTracks, err := bc.addAudioClaims(clientTracks)
	if err != nil {
		return err
	}

	errors := make([]error, 0)
	claimed := 0

	var trackQuality QualityLevel = QualityHigh
	if len(leftTracks) > 4 {
		trackQuality = QualityLow
	} else if len(leftTracks) > 2 {
		trackQuality = QualityMid
	}

	for _, clientTrack := range leftTracks {
		if clientTrack.Kind() == webrtc.RTPCodecTypeVideo {

			// glog.Info("bitratecontroller: track ", clientTrack.ID(), " quality ", trackQuality)
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
	bc.mu.Lock()
	defer bc.mu.Unlock()

	bc.claims[clientTrack.ID()] = &bitrateClaim{
		mu:        sync.RWMutex{},
		track:     clientTrack,
		quality:   quality,
		simulcast: clientTrack.IsSimulcast(),
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

func (bc *bitrateController) totalSentBitrates() uint32 {
	total := uint32(0)

	for _, claim := range bc.Claims() {
		total += claim.track.SendBitrate()
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
		// overshoot the bandwidth to allow test the bandwidth
		bw = int(float64(bw) * 1.3)

		bc.mu.Lock()
		bc.targetBitrate = uint32(bw)
		bc.mu.Unlock()
	})
}

func (bc *bitrateController) loopMonitor() {
	ctx, cancel := context.WithCancel(bc.client.Context())
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			var needAdjustment bool

			bc.mu.RLock()
			totalSendBitrates := bc.totalSentBitrates()
			bw := bc.targetBitrate
			bc.mu.RUnlock()

			// glog.Info("bitratecontroller: available bandwidth ", ThousandSeparator(int(bw)), " total bitrate ", ThousandSeparator(int(totalSendBitrates)))

			availableBw := uint32(bw) - totalSendBitrates

			if totalSendBitrates < uint32(bw) {
				needAdjustment = bc.canIncreaseBitrate(availableBw)
			} else {
				needAdjustment = bc.canDecreaseBitrate()
			}

			if !needAdjustment {
				time.Sleep(3 * time.Second)
				continue
			}

			// glog.Info("bitratecontroller: available bandwidth ", ThousandSeparator(int(bw)), " total bitrate ", ThousandSeparator(int(totalSendBitrates)))

			bc.fitBitratesToBandwidth(uint32(bw))

			bc.mu.Lock()
			bc.lastBitrateAdjustmentTS = time.Now()
			bc.mu.Unlock()

			time.Sleep(3 * time.Second)
		}
	}

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
					oldBitrate := claim.SendBitrate()
					if oldBitrate == 0 {
						continue
					}

					newQuality := QualityLevel(i) - 1
					newBitrate := claim.QualityLevelToBitrate(newQuality)
					bitrateGap := oldBitrate - newBitrate
					// glog.Info("bitratecontroller: reduce bitrate for track ", claim.track.ID(), " from ", claim.Quality(), " to ", newQuality)
					bc.setQuality(claim.track.ID(), newQuality)

					claim.track.RequestPLI()
					totalSentBitrates = totalSentBitrates - bitrateGap

					// glog.Info("bitratecontroller: total sent bitrates ", ThousandSeparator(int(totalSentBitrates)), " bandwidth ", ThousandSeparator(int(bw)))

					// check if the reduced bitrate is fit to the available bandwidth
					if totalSentBitrates <= bw {
						// glog.Info("bitratecontroller: reduce sent bitrates ", ThousandSeparator(int(totalSentBitrates)), " to bandwidth ", ThousandSeparator(int(bw)))
						return
					}
				}
			}
		}
	} else if totalSentBitrates < bw {
		// increase bitrates
		for i := QualityLow; i < QualityHigh; i++ {
			for _, claim := range claims {
				if claim.IsAdjustable() &&
					claim.Quality() == QualityLevel(i) {
					oldBitrate := claim.SendBitrate()
					if oldBitrate == 0 {
						continue
					}

					newQuality := QualityLevel(i) + 1
					newBitrate := claim.QualityLevelToBitrate(newQuality)
					bitrateIncrease := newBitrate - oldBitrate

					// check if the bitrate increase will more than the available bandwidth
					if totalSentBitrates+bitrateIncrease >= bw {
						// glog.Info("bitratecontroller: increase sent bitrates ", ThousandSeparator(int(totalSentBitrates)), " to bandwidth ", ThousandSeparator(int(bw)))
						return
					}

					// glog.Info("bitratecontroller: increase bitrate for track ", claim.track.ID(), " from ", claim.Quality(), " to ", claim.Quality()+1)
					bc.setQuality(claim.track.ID(), newQuality)
					// update current total bitrates
					totalSentBitrates = totalSentBitrates + bitrateIncrease
					// glog.Info("bitratecontroller: total sent bitrates ", ThousandSeparator(int(totalSentBitrates)), " bandwidth ", ThousandSeparator(int(bw)))
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

	// TODO: check if it is necessary to set max quality to none
	if videoSize.Width == 0 || videoSize.Height == 0 {
		glog.Info("bitrate: track ", videoSize.TrackID, " video size is 0, set max quality to low")
		claim.track.SetMaxQuality(QualityLow)
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

	nextBitrate := claim.QualityLevelToBitrate(nextQuality)
	currentBitrate := claim.SendBitrate()

	bandwidthGap := nextBitrate - currentBitrate

	return bandwidthGap < bandwidthLeft
}
