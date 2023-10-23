package sfu

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"
)

var roomManager *Manager

func TestMain(m *testing.M) {
	// flag.Set("logtostderr", "true")
	// flag.Set("stderrthreshold", "INFO")

	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	turnServer := StartTurnServer(ctx, "127.0.0.1")
	defer turnServer.Close()

	// create room manager first before create new room
	roomManager = NewManager(ctx, "test", Options{
		WebRTCPort:               40004,
		ConnectRemoteRoomTimeout: 30 * time.Second,
		IceServers:               DefaultTestIceServers(),
	})

	result := m.Run()

	os.Exit(result)
}
