package sfu

import (
	"context"
	"flag"
	"os"
	"testing"

	"github.com/pion/logging"
)

var TestLogger logging.LeveledLogger

func TestMain(m *testing.M) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	os.Setenv("stderrthreshold", "WARNING")
	os.Setenv("PIONS_LOG_TRACE", "sfu,vad,ice")
	os.Setenv("PIONS_LOG_DEBUG", "sfu,vad,ice")
	// os.Setenv("PIONS_LOG_INFO", "sfu,vad")
	os.Setenv("PIONS_LOG_WARN", "webrtc,ice,sfu,vad")
	os.Setenv("PIONS_LOG_ERROR", "webrtc,ice,sfu,vad")

	flag.Parse()

	TestLogger = logging.NewDefaultLoggerFactory().NewLogger("sfu")

	StartTurnServer(ctx, "127.0.0.1")

	result := m.Run()

	os.Exit(result)
}
