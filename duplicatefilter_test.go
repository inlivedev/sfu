package sfu

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDuplicateFilterDropsRepeatedPackets(t *testing.T) {
	f := &duplicateFilter{}

	require.False(t, f.isDuplicate(9210))
	require.False(t, f.isDuplicate(9211))
	require.False(t, f.isDuplicate(9212))

	// RTX probe padding resends the same packet over and over
	for i := 0; i < 150; i++ {
		require.True(t, f.isDuplicate(9212))
		require.True(t, f.isDuplicate(9211))
	}

	require.False(t, f.isDuplicate(9213))
}

func TestDuplicateFilterPassesRecoveredPackets(t *testing.T) {
	f := &duplicateFilter{}

	require.False(t, f.isDuplicate(100))
	// 101 and 102 are lost
	require.False(t, f.isDuplicate(103))
	require.False(t, f.isDuplicate(104))

	// the retransmissions are forwarded once
	require.False(t, f.isDuplicate(102))
	require.False(t, f.isDuplicate(101))
	require.True(t, f.isDuplicate(101))
	require.True(t, f.isDuplicate(102))
}

func TestDuplicateFilterWrapAround(t *testing.T) {
	f := &duplicateFilter{}

	require.False(t, f.isDuplicate(65534))
	require.False(t, f.isDuplicate(65535))
	require.False(t, f.isDuplicate(1)) // 0 is lost
	require.True(t, f.isDuplicate(65535))
	require.False(t, f.isDuplicate(0))
	require.True(t, f.isDuplicate(0))
	require.False(t, f.isDuplicate(2))
}

func TestDuplicateFilterForgetsSlotsWhenAdvancing(t *testing.T) {
	f := &duplicateFilter{}

	require.False(t, f.isDuplicate(10))
	// one full window later the bitmap slot of 10 is reused by 10+window
	require.False(t, f.isDuplicate(10+duplicateFilterWindow-1))
	require.False(t, f.isDuplicate(10+duplicateFilterWindow+1))
	// 10+window was skipped, its slot must not still hold 10
	require.False(t, f.isDuplicate(10+duplicateFilterWindow))
}

func TestDuplicateFilterTooLateAndDiscontinuity(t *testing.T) {
	f := &duplicateFilter{}

	require.False(t, f.isDuplicate(5000))
	require.False(t, f.isDuplicate(7000))

	// older than the window: too late to be useful
	require.True(t, f.isDuplicate(7000-duplicateFilterWindow))

	// far behind: the stream restarted with a new sequence space
	highest := uint16(7000)
	restart := highest - duplicateFilterResetGap
	require.False(t, f.isDuplicate(restart))
	require.False(t, f.isDuplicate(restart+1))
	require.True(t, f.isDuplicate(restart+1))

	// far ahead resets as well
	require.False(t, f.isDuplicate(40000))
	require.False(t, f.isDuplicate(39999))
}
