package voiceactivedetector

import (
	"context"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
)

type VoicePacketData struct {
	SequenceNo uint16 `json:"sequenceNo"`
	Timestamp  uint32 `json:"timestamp"`
	AudioLevel uint8  `json:"audioLevel"`
}

type VoiceActivity struct {
	TrackID     string             `json:"trackID"`
	StreamID    string             `json:"streamID"`
	SSRC        uint32             `json:"ssrc"`
	ClockRate   uint32             `json:"clockRate"`
	AudioLevels []*VoicePacketData `json:"audioLevels"`
}

type VoiceDetector struct {
	streamInfo     *interceptor.StreamInfo
	config         Config
	streamID       string
	trackID        string
	context        context.Context
	cancel         context.CancelFunc
	detected       bool
	startDetected  uint32
	lastDetectedTS uint32
	channel        chan *RetainablePacket
	mu             sync.RWMutex
	VoicePackets   []*RetainablePacket
	packetManager  *PacketManager
	callback       func(activity VoiceActivity)
	log            logging.LeveledLogger
}

func newVAD(ctx context.Context, config Config, streamInfo *interceptor.StreamInfo, log logging.LeveledLogger) *VoiceDetector {
	v := &VoiceDetector{
		context:       ctx,
		config:        config,
		streamInfo:    streamInfo,
		channel:       make(chan *RetainablePacket),
		mu:            sync.RWMutex{},
		VoicePackets:  make([]*RetainablePacket, 0),
		packetManager: newPacketManager(),
		log:           log,
	}

	v.run()

	return v
}

// run goroutine to process packets and detect voice
// we keep the packets within the head and tail timestamp margin based on packet timestamp
// if voice not detected, we drop the packets after out from head margin
// once voice detected, we keep the packets from the head margin until the tail margin
// when the voice detected, we send all the packets to callback through channel
// keep send all incoming packets to callback until the tail margin close
// once the tail margin close, stop send the packet.
func (v *VoiceDetector) run() {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		ctx, cancel := context.WithCancel(v.context)
		v.cancel = cancel

		defer func() {
			ticker.Stop()
			cancel()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case voicePacket := <-v.channel:
				if v.isDetected(voicePacket) {
					// send all packets to callback
					v.sendPacketsToCallback()
				}
			case <-ticker.C:
				go v.dropExpiredPackets()
			}
		}
	}()
}

func (v *VoiceDetector) dropExpiredPackets() {
loop:
	for {
		v.mu.Lock()
		if len(v.VoicePackets) == 0 {
			v.mu.Unlock()
			break loop
		}

		lastPacket := v.VoicePackets[len(v.VoicePackets)-1]

		packet := v.VoicePackets[0]
		if packet.Data().Timestamp*1000/v.streamInfo.ClockRate+uint32(v.config.HeadMargin.Milliseconds()) < lastPacket.Data().Timestamp*1000/v.streamInfo.ClockRate {
			// release and packet
			packet.Release()
			v.VoicePackets = v.VoicePackets[1:]
		} else {
			v.mu.Unlock()
			break loop
		}
		v.mu.Unlock()
	}
}

func (v *VoiceDetector) sendPacketsToCallback() {
	if v.callback == nil {
		return
	}

	// get all packets from head margin until tail margin

	packets := v.getPackets()

	v.onVoiceDetected(VoiceActivity{
		TrackID:     v.trackID,
		StreamID:    v.streamID,
		SSRC:        v.streamInfo.SSRC,
		ClockRate:   v.streamInfo.ClockRate,
		AudioLevels: packets,
	})

	// clear packets
	v.clearPackets()

}

func (v *VoiceDetector) clearPackets() {
	v.mu.Lock()
	defer v.mu.Unlock()

	for _, packet := range v.VoicePackets {
		packet.Release()
	}

	v.VoicePackets = make([]*RetainablePacket, 0)
}

func (v *VoiceDetector) getPackets() []*VoicePacketData {
	v.mu.RLock()
	defer v.mu.RUnlock()

	var packets []*VoicePacketData
	for _, packet := range v.VoicePackets {
		packets = append(packets, packet.Data())
	}

	return packets
}

func (v *VoiceDetector) onVoiceDetected(activity VoiceActivity) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.callback != nil {
		v.callback(activity)
	}
}

func (v *VoiceDetector) OnVoiceDetected(callback func(VoiceActivity)) {
	if v == nil {
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.callback = callback
}

func (v *VoiceDetector) isDetected(vp *RetainablePacket) bool {
	v.mu.RLock()
	v.VoicePackets = append(v.VoicePackets, vp)
	v.mu.RUnlock()

	clockRate := v.streamInfo.ClockRate

	isThresholdPassed := vp.Data().AudioLevel < v.config.Threshold

	// check if voice detected
	if !v.detected && v.startDetected == 0 && isThresholdPassed {
		v.startDetected = vp.Data().Timestamp

		return v.detected
	}

	isHeadMarginPassed := vp.Data().Timestamp*1000/clockRate > (v.startDetected*1000/clockRate)+uint32(v.config.HeadMargin.Milliseconds())

	isTailMarginPassedAfterStarted := vp.Data().Timestamp*1000/clockRate > (v.startDetected*1000/clockRate)+uint32(v.config.TailMargin.Milliseconds())

	// rest start detected timestamp if audio level above threshold after previously start detected
	if !v.detected && v.startDetected != 0 && isTailMarginPassedAfterStarted && !isThresholdPassed {
		v.startDetected = 0
		return v.detected
	}

	// detected true after the audio level stay below threshold until pass the head margin
	if !v.detected && v.startDetected != 0 && isHeadMarginPassed {
		// start send packet to callback
		v.detected = true
		v.lastDetectedTS = vp.Data().Timestamp

		return v.detected
	}

	isTailMarginPassed := vp.Data().Timestamp*1000/clockRate > (v.lastDetectedTS*1000/clockRate)+uint32(v.config.TailMargin.Milliseconds())

	if v.detected && !isThresholdPassed && isTailMarginPassed {
		// stop send packet to callback
		v.detected = false
		v.startDetected = 0
		v.onVoiceDetected(VoiceActivity{
			TrackID:     v.trackID,
			StreamID:    v.streamID,
			SSRC:        v.streamInfo.SSRC,
			ClockRate:   v.streamInfo.ClockRate,
			AudioLevels: nil,
		})
		return v.detected
	}

	if v.detected && isThresholdPassed {
		// keep send packets to callback
		v.lastDetectedTS = vp.Data().Timestamp
	}

	return v.detected
}

func (v *VoiceDetector) addPacket(header *rtp.Header, audioLevel uint8) {
	vp, err := v.packetManager.NewPacket(header.SequenceNumber, header.Timestamp, audioLevel)
	if err != nil {
		v.log.Errorf("failed to create new packet: %v", err)
		return
	}
	v.channel <- vp
}

func (v *VoiceDetector) UpdateTrack(trackID, streamID string) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	v.trackID = trackID
	v.streamID = streamID
}

func (v *VoiceDetector) Stop() {
	v.cancel()
}

func (v *VoiceDetector) updateStreamInfo(streamInfo *interceptor.StreamInfo) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	v.streamInfo = streamInfo
}
