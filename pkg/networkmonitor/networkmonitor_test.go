package networkmonitor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNetworkMonitor(t *testing.T) {
	orderedSeqs := []uint16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 12}
	unorderedSeqs := []uint16{13, 15, 20, 14, 16, 17, 18, 19, 27, 28, 29, 30, 31, 21, 22, 23, 24, 25, 26, 32, 33, 34, 35}
	unorderedSeqs2 := []uint16{36, 41, 42, 43, 44, 37, 38, 39, 40, 45}
	maxLatency := time.Millisecond * 100
	stableDuration := time.Second * 2

	nm := New(maxLatency, stableDuration)

	var state NetworkConditionType

	nm.OnNetworkConditionChanged(func(condition NetworkConditionType) {
		state = condition
	})

	// Test adding packets
	for _, seq := range orderedSeqs {
		err := nm.Add(seq)
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}

	assert.Equal(t, STABLE, state)

	// Test adding unordered packets
	for _, seq := range unorderedSeqs {
		err := nm.Add(seq)
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}

	assert.Equal(t, STABLE, state)

	// Test adding unordered packets with timeout
	for _, seq := range unorderedSeqs2 {
		err := nm.Add(seq)
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}

		time.Sleep(maxLatency + (time.Millisecond * 10))
	}

	assert.Equal(t, UNSTABLE, state)
}
