package voiceactivedetector

import (
	"context"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
)

func BenchmarkVAD(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leveledLogger := logging.NewDefaultLoggerFactory().NewLogger("sfu")
	intc := new(ctx, leveledLogger)
	vad := newVAD(ctx, intc.config, &interceptor.StreamInfo{
		ID:        "streamID",
		ClockRate: 48000,
	})

	vad.OnVoiceDetected(func([]VoicePacketData) {
		// Do nothing
	})

	header := &rtp.Header{}
	for i := 0; i < b.N; i++ {
		vad.addPacket(header, 3, true)
	}

}
