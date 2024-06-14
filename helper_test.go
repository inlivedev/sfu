package sfu

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
)

type PeerClient struct {
	PeerConnection  *webrtc.PeerConnection
	PendingTracks   []*webrtc.TrackLocalStaticSample
	ID              string
	RelayClient     *Client
	NeedRenegotiate bool
	InRenegotiation bool
}

type RemoteTrackTest struct {
	Track  *webrtc.TrackRemote
	Client *PeerClient
}

func DefaultTestIceServers() []webrtc.ICEServer {
	return []webrtc.ICEServer{}
}

func CheckRoutines(t *testing.T) func() {
	tryLoop := func(failMessage string) {
		try := 0
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			runtime.GC()
			routines := getRoutines()
			if len(routines) == 0 {
				return
			}
			if try >= 50 {
				t.Fatalf("%s: \n%s", failMessage, strings.Join(routines, "\n\n")) // nolint
			}
			try++
		}
	}

	tryLoop("Unexpected routines on test startup")
	return func() {
		tryLoop("Unexpected routines on test end")
	}
}

func getRoutines() []string {
	buf := make([]byte, 2<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return filterRoutines(strings.Split(string(buf), "\n\n"))
}

func filterRoutines(routines []string) []string {
	result := []string{}
	for _, stack := range routines {
		if stack == "" || // Empty
			filterRoutineWASM(stack) || // WASM specific exception
			strings.Contains(stack, "sfu.TestMain(") || // Tests
			strings.Contains(stack, "testing.(*T).Run(") || // Test run
			strings.Contains(stack, "sfu.getRoutines(") { // This routine

			continue
		}
		result = append(result, stack)
	}
	return result
}

func filterRoutineWASM(string) bool {
	return false
}
