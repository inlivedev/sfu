package sfu

import (
	"flag"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")

	result := m.Run()

	os.Exit(result)
}
