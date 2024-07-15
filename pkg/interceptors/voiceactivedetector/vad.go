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
	IsVoice    bool   `json:"isVoice"`
}

type VoiceActivity struct {
	TrackID     string            `json:"trackID"`
	StreamID    string            `json:"streamID"`
	SSRC        uint32            `json:"ssrc"`
	ClockRate   uint32            `json:"clockRate"`
	AudioLevels []VoicePacketData `json:"audioLevels"`
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
	channel        chan VoicePacketData
	mu             sync.RWMutex
	VoicePackets   []VoicePacketData
	callback       func(activity VoiceActivity)
	log            logging.LeveledLogger
}

func newVAD(ctx context.Context, config Config, streamInfo *interceptor.StreamInfo) *VoiceDetector {
	v := &VoiceDetector{
		context:      ctx,
		config:       config,
		streamInfo:   streamInfo,
		channel:      make(chan VoicePacketData, 1024),
		mu:           sync.RWMutex{},
		VoicePackets: make([]VoicePacketData, 0),
		log:          logging.NewDefaultLoggerFactory().NewLogger("vad"),
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
		ticker := time.NewTicker(v.config.TailMargin)
		ctx, cancel := context.WithCancel(v.context)
		v.cancel = cancel

		defer func() {
			ticker.Stop()
			cancel()
		}()

		active := false
		lastSent := time.Now()

		for {
			select {
			case <-ctx.Done():
				return
			case voicePacket := <-v.channel:
				if voicePacket.AudioLevel < v.config.Threshold {
					// send all packets to callback
					activity := VoiceActivity{
						TrackID:     v.trackID,
						StreamID:    v.streamID,
						SSRC:        v.streamInfo.SSRC,
						ClockRate:   v.streamInfo.ClockRate,
						AudioLevels: []VoicePacketData{voicePacket},
					}

					v.onVoiceDetected(activity)
					lastSent = time.Now()
					active = true
				}
			case <-ticker.C:
				if active && time.Since(lastSent) > v.config.TailMargin {
					// we need to notify that the voice is stopped
					v.onVoiceDetected(VoiceActivity{
						TrackID:     v.trackID,
						StreamID:    v.streamID,
						SSRC:        v.streamInfo.SSRC,
						ClockRate:   v.streamInfo.ClockRate,
						AudioLevels: nil,
					})
					active = false
				}
			}
		}
	}()
}

// TODO: this function is use together with isDetected function
// need to fix isDetected function first before we can use this function
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
		if packet.Timestamp*1000/v.streamInfo.ClockRate+uint32(v.config.HeadMargin.Milliseconds()) < lastPacket.Timestamp*1000/v.streamInfo.ClockRate {
			// release and packet
			v.VoicePackets = v.VoicePackets[1:]
		} else {
			v.mu.Unlock()
			break loop
		}
		v.mu.Unlock()
	}
}

func (v *VoiceDetector) sendPacketsToCallback() int {
	if v.callback == nil {
		return 0
	}

	// get all packets from head margin until tail margin

	packets := v.getPackets()

	length := len(packets)

	if length > 0 {
		activity := VoiceActivity{
			TrackID:     v.trackID,
			StreamID:    v.streamID,
			SSRC:        v.streamInfo.SSRC,
			ClockRate:   v.streamInfo.ClockRate,
			AudioLevels: packets,
		}

		v.onVoiceDetected(activity)
	}

	// clear packets
	v.clearPackets()

	return length
}

func (v *VoiceDetector) clearPackets() {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.VoicePackets = make([]VoicePacketData, 0)
}

func (v *VoiceDetector) getPackets() []VoicePacketData {
	v.mu.RLock()
	defer v.mu.RUnlock()

	var packets []VoicePacketData
	for _, packet := range v.VoicePackets {
		if packet.AudioLevel < v.config.Threshold {
			packets = append(packets, packet)
		}
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

// TODO: this function is sometimes stop detecting the voice activity
// we need to investigate why this is happening
// for now we're fallback to threshold based detection
func (v *VoiceDetector) isDetected(vp VoicePacketData) bool {
	v.mu.RLock()
	v.VoicePackets = append(v.VoicePackets, vp)
	v.mu.RUnlock()

	clockRate := v.streamInfo.ClockRate

	isThresholdPassed := vp.AudioLevel < v.config.Threshold

	// check if voice detected
	if !v.detected && v.startDetected == 0 && isThresholdPassed {
		v.startDetected = vp.Timestamp

		return v.detected
	}

	currentTS := vp.Timestamp
	durationGap := (currentTS - v.startDetected) * 1000 / clockRate

	isTailMarginPassedAfterStarted := durationGap > uint32(v.config.TailMargin.Milliseconds())

	// restart detected timestamp if audio level above threshold after previously start detected
	if !v.detected && v.startDetected != 0 && isTailMarginPassedAfterStarted && !isThresholdPassed {
		v.startDetected = 0
		return v.detected
	}

	isHeadMarginPassed := durationGap > uint32(v.config.HeadMargin.Milliseconds())

	// detected true after the audio level stay below threshold until pass the head margin
	if !v.detected && v.startDetected != 0 && isHeadMarginPassed {
		// start send packet to callback
		v.detected = true
		v.lastDetectedTS = vp.Timestamp

		return v.detected
	}

	isTailMarginPassed := vp.Timestamp*1000/clockRate > (v.lastDetectedTS*1000/clockRate)+uint32(v.config.TailMargin.Milliseconds())

	if v.detected && !isThresholdPassed && isTailMarginPassed {
		// stop send packet to callback
		v.detected = false
		v.startDetected = 0

		return v.detected
	}

	if v.detected && isThresholdPassed {
		// keep send packets to callback
		v.lastDetectedTS = vp.Timestamp
	}

	return v.detected
}

func (v *VoiceDetector) addPacket(header *rtp.Header, audioLevel uint8, isVoice bool) {
	vp := VoicePacketData{
		SequenceNo: header.SequenceNumber,
		Timestamp:  header.Timestamp,
		AudioLevel: audioLevel,
		IsVoice:    isVoice,
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
