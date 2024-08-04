package sfu

import (
	"context"
	"flag"
	"os"
	"testing"

	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

var TestLogger logging.LeveledLogger

var sfuOpts Options

func TestMain(m *testing.M) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	os.Setenv("stderrthreshold", "DEBUG")
	// os.Setenv("PIONS_LOG_TRACE", "sfu,vad,ice")
	os.Setenv("PIONS_LOG_DEBUG", "sfu,vad")
	// os.Setenv("PIONS_LOG_INFO", "sfu,vad")
	os.Setenv("PIONS_LOG_WARN", "sfu,vad")
	os.Setenv("PIONS_LOG_ERROR", "sfu,vad")

	flag.Parse()

	TestLogger = logging.NewDefaultLoggerFactory().NewLogger("sfu")

	StartStunServer(ctx, "127.0.0.1")

	sfuOpts = DefaultOptions()
	sfuOpts.IceServers = DefaultTestIceServers()

	sfuOpts.SettingEngine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	sfuOpts.SettingEngine.SetIncludeLoopbackCandidate(true)

	result := m.Run()

	os.Exit(result)
}
