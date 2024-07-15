package sfu

import (
	"flag"
	"os"
	"testing"

	"github.com/pion/logging"
)

var TestLogger logging.LeveledLogger

func TestMain(m *testing.M) {
	os.Setenv("stderrthreshold", "INFO")
	os.Setenv("PIONS_LOG_TRACE", "sfu,vad")
	// os.Setenv("PIONS_LOG_DEBUG", "sfu,vad")
	os.Setenv("PIONS_LOG_INFO", "sfu,vad")
	os.Setenv("PIONS_LOG_WARN", "sfu,vad")
	os.Setenv("PIONS_LOG_ERROR", "sfu,vad")

	flag.Parse()

	TestLogger = logging.NewDefaultLoggerFactory().NewLogger("sfu")

	result := m.Run()

	os.Exit(result)
}
