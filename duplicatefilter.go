package sfu

const (
	// duplicateFilterWindow is how many sequence numbers behind the newest one
	// the filter remembers. Older packets are too late to be useful.
	duplicateFilterWindow = 1024
	// duplicateFilterResetGap is the distance, forward or backward, that is treated as a
	// sequence number discontinuity rather than a late or duplicate packet.
	duplicateFilterResetGap = 8 * 1024
)

// duplicateFilter detects RTP packets that were already received on a stream.
//
// Pion delivers RTX packets on the media track with their original sequence number.
// Besides NACK responses, Chrome sends old media packets over RTX as bandwidth probe
// padding, so the same packet can arrive many times. Those copies must not be
// forwarded to subscribers. Genuine retransmissions of packets that were lost
// still pass. It is not safe for concurrent use.
type duplicateFilter struct {
	init    bool
	highest uint16
	window  [duplicateFilterWindow / 64]uint64
}

// isDuplicate records seq and reports whether the packet should be dropped,
// because it was already seen or it is too late to be useful.
func (f *duplicateFilter) isDuplicate(seq uint16) bool {
	if !f.init {
		f.reset(seq)
		return false
	}

	diff := int(int16(seq - f.highest))

	switch {
	case diff == 0:
		return true
	case diff > 0:
		if diff >= duplicateFilterResetGap {
			f.reset(seq)
			return false
		}

		if diff >= duplicateFilterWindow {
			f.window = [duplicateFilterWindow / 64]uint64{}
		} else {
			for s := f.highest + 1; s != seq; s++ {
				f.clear(s)
			}
		}

		f.highest = seq
		f.set(seq)

		return false
	default:
		back := -diff
		if back >= duplicateFilterResetGap {
			f.reset(seq)
			return false
		}

		if back >= duplicateFilterWindow {
			return true
		}

		if f.has(seq) {
			return true
		}

		f.set(seq)

		return false
	}
}

func (f *duplicateFilter) reset(seq uint16) {
	f.init = true
	f.highest = seq
	f.window = [duplicateFilterWindow / 64]uint64{}
	f.set(seq)
}

func (f *duplicateFilter) set(seq uint16) {
	i := seq % duplicateFilterWindow
	f.window[i/64] |= 1 << (i % 64)
}

func (f *duplicateFilter) clear(seq uint16) {
	i := seq % duplicateFilterWindow
	f.window[i/64] &^= 1 << (i % 64)
}

func (f *duplicateFilter) has(seq uint16) bool {
	i := seq % duplicateFilterWindow
	return f.window[i/64]&(1<<(i%64)) != 0
}
