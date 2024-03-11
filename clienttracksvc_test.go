package sfu

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeSequence(t *testing.T) {
	seqs := []uint16{65531, 65532, 65533, 65534, 65535, 1, 2, 3, 4, 5}
	dropped := uint16(3000)
	expecteds := []uint16{62531, 62532, 62533, 62534, 62535, 62536, 62537, 62538, 62539, 62540}

	require.Equal(t, len(seqs), len(expecteds), "length of seqs and expecteds should be the same")

	for i, seq := range seqs {
		expected := expecteds[i]
		normalizeSeq := normalizeSequenceNumber(seq, dropped)
		require.Equal(t, expected, normalizeSeq, "expected %d got %d", expected, normalizeSeq)
	}

}
