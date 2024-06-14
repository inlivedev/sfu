package sfu

import (
	"flag"
	"os"
	"testing"

	"github.com/pion/logging"
)

var TestLogger logging.LeveledLogger

func TestMain(m *testing.M) {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")

	flag.Parse()

	TestLogger = logging.NewDefaultLoggerFactory().NewLogger("sfu")

	result := m.Run()

	os.Exit(result)
}
