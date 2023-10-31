package voiceactivedetector

import (
	"context"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type VoicePacketData struct {
	SequenceNo uint16
	Timestamp  uint32
	AudioLevel uint8
}

type VoiceDetector struct {
	streamInfo     *interceptor.StreamInfo
	interceptor    *Interceptor
	streamID       string
	trackID        string
	context        context.Context
	cancel         context.CancelFunc
	detected       bool
	lastDetectedTS uint32
	channel        chan VoicePacketData
	mu             sync.Mutex
	VoicePackets   []VoicePacketData
	callbacks      []func(trackID, streamID string, SSRC uint32, voiceData []VoicePacketData)
}

func newVAD(ctx context.Context, i *Interceptor, streamInfo *interceptor.StreamInfo) *VoiceDetector {
	v := &VoiceDetector{
		context:      ctx,
		interceptor:  i,
		streamInfo:   streamInfo,
		channel:      make(chan VoicePacketData),
		mu:           sync.Mutex{},
		VoicePackets: make([]VoicePacketData, 0),
		callbacks:    make([]func(trackID, streamID string, SSRC uint32, voiceData []VoicePacketData), 0),
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
		if packet.Timestamp+uint32(v.interceptor.getConfig().HeadMargin.Milliseconds()) < lastPacket.Timestamp {
			// drop packet
			v.VoicePackets = v.VoicePackets[1:]
		} else {
			break loop
		}
	}
}

func (v *VoiceDetector) sendPacketsToCallback() {
	v.mu.Lock()
	noCallbacks := len(v.callbacks) == 0
	v.mu.Unlock()
	if noCallbacks {
		return
	}

	// get all packets from head margin until tail margin

	packets := v.getPackets()

	v.onVoiceDetected(v.trackID, v.streamID, v.streamInfo.SSRC, packets)

	// clear packets
	v.mu.Lock()
	v.VoicePackets = make([]VoicePacketData, 0)
	v.mu.Unlock()
}

func (v *VoiceDetector) getPackets() []VoicePacketData {
	v.mu.Lock()
	defer v.mu.Unlock()
	packets := make([]VoicePacketData, 0)
	packets = append(packets, v.VoicePackets...)

	return packets
}

func (v *VoiceDetector) onVoiceDetected(trackID, streamID string, SSRC uint32, voiceData []VoicePacketData) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for _, callback := range v.callbacks {
		callback(trackID, streamID, SSRC, voiceData)
	}
}

func (v *VoiceDetector) OnVoiceDetected(callback func(trackID, streamID string, SSRC uint32, voiceData []VoicePacketData)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.callbacks = append(v.callbacks, callback)
}

func (v *VoiceDetector) isDetected(vp VoicePacketData) bool {
	v.mu.Lock()
	v.VoicePackets = append(v.VoicePackets, vp)
	v.mu.Unlock()

	// check if voice detected
	if !v.detected && vp.AudioLevel <= v.interceptor.getConfig().Threshold {
		v.mu.Lock()
		v.lastDetectedTS = vp.Timestamp
		v.detected = true
		v.mu.Unlock()
		return v.detected
	}
	isTailMarginPassed := vp.Timestamp > v.lastDetectedTS+uint32(v.interceptor.getConfig().TailMargin.Milliseconds())
	if v.detected && isTailMarginPassed && vp.AudioLevel > v.interceptor.getConfig().Threshold {
		// stop send packet to callback
		v.mu.Lock()
		v.detected = false
		v.mu.Unlock()
		v.onVoiceDetected(v.trackID, v.streamID, v.streamInfo.SSRC, nil)
	} else if v.detected && !isTailMarginPassed {
		// keep send packets to callback
		v.lastDetectedTS = vp.Timestamp
	}

	return v.detected
}

func (v *VoiceDetector) AddPacket(header *rtp.Header, audioLevel uint8) {
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
