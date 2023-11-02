package voiceactivedetector

import (
	"context"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type VoicePacketData struct {
	SequenceNo uint16 `json:"sequence_no"`
	Timestamp  uint32 `json:"timestamp"`
	AudioLevel uint8  `json:"audio_level"`
}

type VoiceActivity struct {
	TrackID     string            `json:"track_id"`
	StreamID    string            `json:"stream_id"`
	SSRC        uint32            `json:"ssrc"`
	ClockRate   uint32            `json:"clock_rate"`
	AudioLevels []VoicePacketData `json:"audio_levels"`
}

type VoiceDetector struct {
	streamInfo     *interceptor.StreamInfo
	interceptor    *Interceptor
	streamID       string
	trackID        string
	context        context.Context
	cancel         context.CancelFunc
	detected       bool
	startDetected  uint32
	lastDetectedTS uint32
	channel        chan VoicePacketData
	mu             sync.Mutex
	VoicePackets   []VoicePacketData
	callbacks      []func(activity VoiceActivity)
}

func newVAD(ctx context.Context, i *Interceptor, streamInfo *interceptor.StreamInfo) *VoiceDetector {
	v := &VoiceDetector{
		context:      ctx,
		interceptor:  i,
		streamInfo:   streamInfo,
		channel:      make(chan VoicePacketData),
		mu:           sync.Mutex{},
		VoicePackets: make([]VoicePacketData, 0),
		callbacks:    make([]func(VoiceActivity), 0),
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
		ctx, cancel := context.WithCancel(v.context)
		v.cancel = cancel
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case voicePacket := <-v.channel:
				if v.isDetected(voicePacket) {
					// send all packets to callback
					v.sendPacketsToCallback()
				} else {
					// drop packets in queue if pass the head margin
					v.dropExpiredPackets()

				}
			}
		}
	}()
}

func (v *VoiceDetector) dropExpiredPackets() {
	v.mu.Lock()
	defer v.mu.Unlock()

loop:
	for {
		if len(v.VoicePackets) == 0 {
			break loop
		}

		lastPacket := v.VoicePackets[len(v.VoicePackets)-1]

		packet := v.VoicePackets[0]
		if packet.Timestamp*1000/v.streamInfo.ClockRate+uint32(v.interceptor.getConfig().HeadMargin.Milliseconds()) < lastPacket.Timestamp*1000/v.streamInfo.ClockRate {
			// drop packet
			v.VoicePackets = v.VoicePackets[1:]
		} else {
			break loop
		}
	}
}

func (v *VoiceDetector) sendPacketsToCallback() {
	noCallbacks := len(v.callbacks) == 0
	if noCallbacks {
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
	v.VoicePackets = make([]VoicePacketData, 0)
}

func (v *VoiceDetector) getPackets() []VoicePacketData {
	v.mu.Lock()
	defer v.mu.Unlock()
	packets := make([]VoicePacketData, 0)
	packets = append(packets, v.VoicePackets...)

	return packets
}

func (v *VoiceDetector) onVoiceDetected(activity VoiceActivity) {
	for _, callback := range v.callbacks {
		callback(activity)
	}
}

func (v *VoiceDetector) OnVoiceDetected(callback func(VoiceActivity)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.callbacks = append(v.callbacks, callback)
}

func (v *VoiceDetector) isDetected(vp VoicePacketData) bool {
	v.VoicePackets = append(v.VoicePackets, vp)

	clockRate := v.streamInfo.ClockRate

	isThresholdPassed := vp.AudioLevel < v.interceptor.getConfig().Threshold

	// check if voice detected
	if !v.detected && v.startDetected == 0 && isThresholdPassed {
		glog.Info("vad: voice started ", vp.Timestamp*1000/clockRate)
		v.startDetected = vp.Timestamp

		return v.detected
	}

	isHeadMarginPassed := vp.Timestamp*1000/clockRate > (v.startDetected*1000/clockRate)+uint32(v.interceptor.getConfig().HeadMargin.Milliseconds())

	isTailMarginPassedAfterStarted := vp.Timestamp*1000/clockRate > (v.startDetected*1000/clockRate)+uint32(v.interceptor.getConfig().TailMargin.Milliseconds())

	// rest start detected timestamp if audio level above threshold after previously start detected
	if !v.detected && v.startDetected != 0 && isTailMarginPassedAfterStarted && !isThresholdPassed {
		glog.Info("vad: voice stopped before detected ", (vp.Timestamp-v.startDetected)*1000/clockRate)
		v.startDetected = 0
		return v.detected
	}

	// detected true after the audio level stay below threshold until pass the head margin
	if !v.detected && v.startDetected != 0 && isHeadMarginPassed {
		glog.Info("vad: voice detected ", (vp.Timestamp-v.startDetected)*1000/clockRate)
		// start send packet to callback
		v.detected = true
		v.lastDetectedTS = vp.Timestamp

		return v.detected
	}

	isTailMarginPassed := vp.Timestamp*1000/clockRate > (v.lastDetectedTS*1000/clockRate)+uint32(v.interceptor.getConfig().TailMargin.Milliseconds())

	if v.detected && !isThresholdPassed && isTailMarginPassed {
		glog.Info("vad: voice ended")
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
		v.lastDetectedTS = vp.Timestamp
	}

	return v.detected
}

func (v *VoiceDetector) addPacket(header *rtp.Header, audioLevel uint8) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.channel <- VoicePacketData{
		SequenceNo: header.SequenceNumber,
		Timestamp:  header.Timestamp,
		AudioLevel: audioLevel,
	}
}

func (v *VoiceDetector) UpdateTrack(trackID, streamID string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.trackID = trackID
	v.streamID = streamID
}

func (v *VoiceDetector) Stop() {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.cancel()
}

func (v *VoiceDetector) updateStreamInfo(streamInfo *interceptor.StreamInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.streamInfo = streamInfo
	if streamInfo.ID != "" {
		v.trackID = streamInfo.ID
	}
}
