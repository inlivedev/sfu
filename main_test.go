package sfu

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
)

var roomManager *Manager

func TestMain(m *testing.M) {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")

	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager = NewManager(ctx, "test", Options{
		ConnectRemoteRoomTimeout: 30 * time.Second,
		EnableMux:                true,
		EnableBandwidthEstimator: true,
		IceServers:               []webrtc.ICEServer{},
	})

	result := m.Run()

	os.Exit(result)
}
