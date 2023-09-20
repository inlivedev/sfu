package sfu

import (
	"context"
	"flag"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("PIONS_LOG_DEBUG", "all")
	flag.Set("PIONS_LOG_INFO", "all")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	turnServer := StartTurnServer(ctx, "127.0.0.1")
	defer turnServer.Close()

	result := m.Run()

	os.Exit(result)
}
