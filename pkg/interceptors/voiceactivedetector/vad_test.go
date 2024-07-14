package voiceactivedetector

import (
	"context"
	"math/rand"
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

	vad.OnVoiceDetected(func(activity VoiceActivity) {
		// Do nothing
	})

	for i := 0; i < b.N; i++ {
		header := &rtp.Header{}
		vad.addPacket(header, 3, false)
	}

	vad.cancel()

	b.ReportAllocs()

}

func randNumberInRange(min, max int) uint8 {
	return uint8(min + rand.Intn(max-min))
}
