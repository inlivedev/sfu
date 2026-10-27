package pacer

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type sentPacket struct {
	ssrc uint32
	seq  uint16
	size int
	at   time.Time
}

type recorder struct {
	mu   sync.Mutex
	sent []sentPacket
}

func (r *recorder) writer() interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
		r.mu.Lock()
		defer r.mu.Unlock()

		n := header.MarshalSize() + len(payload)
		r.sent = append(r.sent, sentPacket{ssrc: header.SSRC, seq: header.SequenceNumber, size: n, at: time.Now()})

		return n, nil
	})
}

func (r *recorder) snapshot() []sentPacket {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]sentPacket(nil), r.sent...)
}

func (r *recorder) bytes() int {
	total := 0
	for _, s := range r.snapshot() {
		total += s.size
	}

	return total
}

func newTestPacer(t *testing.T, bitrate int) *LeakyBucketPacer {
	t.Helper()

	p := NewLeakyBucketPacer(logging.NewDefaultLoggerFactory().NewLogger("pacer"), bitrate, false)
	t.Cleanup(func() { _ = p.Close() })

	return p
}

func writePackets(t *testing.T, p *LeakyBucketPacer, ssrc uint32, count, payloadSize int) {
	t.Helper()

	payload := make([]byte, payloadSize)
	for i := 0; i < count; i++ {
		_, err := p.Write(&rtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: uint16(i)}, payload, nil)
		require.NoError(t, err)
	}
}

func TestLeakyBucketPacerRespectsTargetBitrate(t *testing.T) {
	const bitrate = 1_000_000

	rec := &recorder{}
	p := newTestPacer(t, bitrate)
	p.AddStream(1, rec.writer())

	// ~1 MB queued, far more than can be sent at 1 Mbps during the test.
	writePackets(t, p, 1, 1000, 1000)

	// The old pacer drained every queue on the first tick.
	time.Sleep(20 * time.Millisecond)
	require.Less(t, rec.bytes(), 20_000, "pacer sent the whole queue at once")

	const window = 500 * time.Millisecond
	start := rec.bytes()
	time.Sleep(window)
	got := rec.bytes() - start

	want := int(float64(bitrate) * window.Seconds() / 8)
	require.InDelta(t, want, got, float64(want)*0.25, "paced %d bytes in %v, want ~%d", got, window, want)
}

func TestLeakyBucketPacerSetTargetBitrate(t *testing.T) {
	rec := &recorder{}
	p := newTestPacer(t, 100_000)
	p.AddStream(1, rec.writer())

	// SetTargetBitrate allows p.f (1.5x) headroom over the requested rate.
	p.SetTargetBitrate(2_000_000)
	writePackets(t, p, 1, 1000, 1000)

	const window = 400 * time.Millisecond
	time.Sleep(20 * time.Millisecond)
	start := rec.bytes()
	time.Sleep(window)
	got := rec.bytes() - start

	want := int(1.5 * 2_000_000 * window.Seconds() / 8)
	require.InDelta(t, want, got, float64(want)*0.25, "paced %d bytes in %v, want ~%d", got, window, want)
}

func TestLeakyBucketPacerRoundRobin(t *testing.T) {
	rec := &recorder{}
	p := newTestPacer(t, 1_000_000)
	p.AddStream(1, rec.writer())
	p.AddStream(2, rec.writer())

	writePackets(t, p, 1, 200, 1000)
	writePackets(t, p, 2, 200, 1000)

	time.Sleep(300 * time.Millisecond)

	counts := map[uint32]int{}
	lastSeq := map[uint32]int{1: -1, 2: -1}
	for _, s := range rec.snapshot() {
		counts[s.ssrc]++
		require.Greater(t, int(s.seq), lastSeq[s.ssrc], "packets out of order for ssrc %d", s.ssrc)
		lastSeq[s.ssrc] = int(s.seq)
	}

	require.Greater(t, counts[1], 10)
	require.Greater(t, counts[2], 10)
	require.InDelta(t, counts[1], counts[2], 2, "streams not served round-robin: %v", counts)
	require.Less(t, counts[1]+counts[2], 400, "pacer sent everything at once")
}

func TestLeakyBucketPacerOversizedPayload(t *testing.T) {
	rec := &recorder{}
	p := newTestPacer(t, 1_000_000)
	p.AddStream(1, rec.writer())

	// Queue a valid packet first, so the oversized one would reach the
	// sequence-ordering path that used to dereference the nil packet.
	_, err := p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 1}, make([]byte, 100), nil)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		n, err := p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 2}, make([]byte, 1500), nil)
		require.ErrorIs(t, err, ErrPacketRejected)
		require.Zero(t, n)
	})

	// Oversized payload into an empty queue: previously queued a nil packet
	// that panicked in Run.
	_, err = p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 3}, make([]byte, 100), nil)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)
	require.NotPanics(t, func() {
		_, err := p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 4}, make([]byte, 64*1024), nil)
		require.ErrorIs(t, err, ErrPacketRejected)
	})

	time.Sleep(30 * time.Millisecond)
	sent := rec.snapshot()
	require.Len(t, sent, 2)
	require.Equal(t, uint16(1), sent[0].seq)
	require.Equal(t, uint16(3), sent[1].seq)
}

func TestLeakyBucketPacerUnknownSSRC(t *testing.T) {
	p := newTestPacer(t, 1_000_000)

	_, err := p.Write(&rtp.Header{SSRC: 42}, make([]byte, 100), nil)
	require.True(t, errors.Is(err, ErrNoQueueFound))
}

func TestLeakyBucketPacerPacketLargerThanBudgetDoesNotStall(t *testing.T) {
	// 8 kbps is 5 bytes per tick, far below one packet. The pacer must still
	// send, then pay the debt off before sending the next packet.
	rec := &recorder{}
	p := newTestPacer(t, 8_000)
	p.AddStream(1, rec.writer())

	writePackets(t, p, 1, 2, 1400)

	time.Sleep(100 * time.Millisecond)
	require.Len(t, rec.snapshot(), 1, "first packet should not stall behind the budget")

	time.Sleep(300 * time.Millisecond)
	require.Len(t, rec.snapshot(), 1, "second packet should wait for the debt to be repaid")
}

func TestLeakyBucketPacerIdleDoesNotBankBudget(t *testing.T) {
	const bitrate = 1_000_000

	rec := &recorder{}
	p := newTestPacer(t, bitrate)
	p.AddStream(1, rec.writer())

	// Stay idle for a while; without a burst cap this would bank ~60 KB.
	time.Sleep(500 * time.Millisecond)
	writePackets(t, p, 1, 200, 1000)
	time.Sleep(20 * time.Millisecond)

	require.Less(t, rec.bytes(), 10_000, "pacer burst out budget banked while idle")
}

type fakeResend struct {
	mu       sync.Mutex
	retained int
	released []bool
}

func (f *fakeResend) Retain() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retained++
}

func (f *fakeResend) Release(sent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, sent)
}

func (f *fakeResend) state() (int, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.retained, append([]bool(nil), f.released...)
}

type fakeResendKey struct{}

func TestLeakyBucketPacerTracksResendCompletion(t *testing.T) {
	rec := &recorder{}
	p := newTestPacer(t, 1_000_000)
	p.AddStream(1, rec.writer())

	sentResend := &fakeResend{}
	_, err := p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 7}, make([]byte, 100),
		interceptor.Attributes{fakeResendKey{}: sentResend})
	require.NoError(t, err)

	// A duplicate is dropped and must report that it was not sent.
	dupResend := &fakeResend{}
	_, err = p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 7}, make([]byte, 100),
		interceptor.Attributes{fakeResendKey{}: dupResend})
	require.NoError(t, err)

	retained, released := dupResend.state()
	require.Equal(t, 1, retained)
	require.Equal(t, []bool{false}, released)

	retained, released = sentResend.state()
	require.Equal(t, 1, retained)
	require.Empty(t, released, "resend completed before the pacer sent it")

	require.Eventually(t, func() bool {
		_, released := sentResend.state()
		return len(released) == 1 && released[0]
	}, time.Second, 5*time.Millisecond)
}

func TestLeakyBucketPacerCloseFlushes(t *testing.T) {
	rec := &recorder{}
	p := NewLeakyBucketPacer(logging.NewDefaultLoggerFactory().NewLogger("pacer"), 8_000, false)
	p.AddStream(1, rec.writer())

	writePackets(t, p, 1, 5, 1000)
	require.NoError(t, p.Close())
	require.NoError(t, p.Close())

	require.Len(t, rec.snapshot(), 5)

	_, err := p.Write(&rtp.Header{SSRC: 1}, make([]byte, 100), nil)
	require.ErrorIs(t, err, ErrNoQueueFound)
}
