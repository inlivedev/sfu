package sfu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAudioLevelURI    = sdp.AudioLevelURI
	testTransportCCURI   = sdp.TransportCCURI
	testPlayoutDelayURI  = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"
	testPublisherMid     = "1"
	testPublisherMidID   = 4
	testPublisherRIDID   = 10
	testPublisherRRIDID  = 11
	testPublisherLevelID = 1
	testPublisherTCCID   = 3
	testPublisherDelayID = 6
)

func testPublisherExtensions() []webrtc.RTPHeaderExtensionParameter {
	return []webrtc.RTPHeaderExtensionParameter{
		{URI: testAudioLevelURI, ID: testPublisherLevelID},
		{URI: testTransportCCURI, ID: testPublisherTCCID},
		{URI: sdp.SDESMidURI, ID: testPublisherMidID},
		{URI: testPlayoutDelayURI, ID: testPublisherDelayID},
		{URI: sdp.SDESRTPStreamIDURI, ID: testPublisherRIDID},
		{URI: sdp.SDESRepairRTPStreamIDURI, ID: testPublisherRRIDID},
	}
}

func testPublisherHeader(t *testing.T) rtp.Header {
	t.Helper()

	h := rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Timestamp: 1, SSRC: 1234}
	require.NoError(t, h.SetExtension(testPublisherLevelID, []byte{0x80}))
	require.NoError(t, h.SetExtension(testPublisherTCCID, []byte{0x00, 0x01}))
	require.NoError(t, h.SetExtension(testPublisherMidID, []byte(testPublisherMid)))
	require.NoError(t, h.SetExtension(testPublisherDelayID, []byte{0x01, 0x02, 0x03}))
	require.NoError(t, h.SetExtension(testPublisherRIDID, []byte("h")))
	require.NoError(t, h.SetExtension(testPublisherRRIDID, []byte("h")))

	return h
}

// newResolvedRewriter returns a rewriter whose table is built for the subscriber, without a transceiver.
func newResolvedRewriter(publisher, subscriber []webrtc.RTPHeaderExtensionParameter, mid string) *headerExtensionRewriter {
	r := newHeaderExtensionRewriter(&Client{}, publisher)
	r.build(subscriber, mid)
	r.resolved = true

	return r
}

func marshalRoundTrip(t *testing.T, h rtp.Header) rtp.Header {
	t.Helper()

	raw, err := (&rtp.Packet{Header: h, Payload: []byte{0x01}}).Marshal()
	require.NoError(t, err)

	p := &rtp.Packet{}
	require.NoError(t, p.Unmarshal(raw))

	return p.Header
}

func TestHeaderExtensionRewriterMapsIDsAndRewritesMid(t *testing.T) {
	subscriber := []webrtc.RTPHeaderExtensionParameter{
		{URI: sdp.SDESMidURI, ID: 9},
		{URI: testAudioLevelURI, ID: 2},
		{URI: testTransportCCURI, ID: 5},
		{URI: sdp.SDESRTPStreamIDURI, ID: 12},
		// playout delay is not negotiated with the subscriber
	}

	r := newResolvedRewriter(testPublisherExtensions(), subscriber, "7")

	original := testPublisherHeader(t)
	// forwarded copies are shallow, like rtppool.CopyPacket
	h := original
	r.rewrite(&h)

	got := marshalRoundTrip(t, h)
	require.Equal(t, uint16(rtp.ExtensionProfileOneByte), got.ExtensionProfile)
	require.ElementsMatch(t, []uint8{9, 2, 5}, got.GetExtensionIDs())
	require.Equal(t, []byte("7"), got.GetExtension(9))
	require.Equal(t, []byte{0x80}, got.GetExtension(2))
	require.Equal(t, []byte{0x00, 0x01}, got.GetExtension(5))
	require.Nil(t, got.GetExtension(12), "rid must not be forwarded")

	// the publisher's packet, shared with other subscribers, is untouched
	require.Len(t, original.Extensions, 6)
	require.Equal(t, []byte(testPublisherMid), original.GetExtension(testPublisherMidID))
	require.Equal(t, []byte("h"), original.GetExtension(testPublisherRIDID))
	require.Equal(t, []byte{0x80}, original.GetExtension(testPublisherLevelID))

	// a packet without extensions still gets the subscriber's mid
	plain := rtp.Header{Version: 2, PayloadType: 96, SSRC: 1234}
	r.rewrite(&plain)
	got = marshalRoundTrip(t, plain)
	require.Equal(t, []uint8{9}, got.GetExtensionIDs())
	require.Equal(t, []byte("7"), got.GetExtension(9))
}

func TestHeaderExtensionRewriterStripsMidWhenNotNegotiated(t *testing.T) {
	subscriber := []webrtc.RTPHeaderExtensionParameter{
		{URI: testTransportCCURI, ID: 5},
	}

	r := newResolvedRewriter(testPublisherExtensions(), subscriber, "7")

	h := testPublisherHeader(t)
	r.rewrite(&h)

	got := marshalRoundTrip(t, h)
	require.Equal(t, []uint8{5}, got.GetExtensionIDs())

	// only MID and RID on the publisher's packet: nothing is left to send
	onlySDES := rtp.Header{Version: 2, PayloadType: 96, SSRC: 1234}
	require.NoError(t, onlySDES.SetExtension(testPublisherMidID, []byte(testPublisherMid)))
	require.NoError(t, onlySDES.SetExtension(testPublisherRIDID, []byte("h")))
	r.rewrite(&onlySDES)
	require.False(t, onlySDES.Extension)

	got = marshalRoundTrip(t, onlySDES)
	require.False(t, got.Extension)
}

func TestHeaderExtensionRewriterUnknownPublisherExtensions(t *testing.T) {
	// relay tracks don't know the publisher's IDs, so only the subscriber's mid is sent
	subscriber := []webrtc.RTPHeaderExtensionParameter{
		{URI: sdp.SDESMidURI, ID: 9},
		{URI: testAudioLevelURI, ID: 2},
	}

	r := newResolvedRewriter(nil, subscriber, "3")

	h := testPublisherHeader(t)
	r.rewrite(&h)

	got := marshalRoundTrip(t, h)
	require.Equal(t, []uint8{9}, got.GetExtensionIDs())
	require.Equal(t, []byte("3"), got.GetExtension(9))
}

func TestHeaderExtensionRewriterTwoByteIDs(t *testing.T) {
	subscriber := []webrtc.RTPHeaderExtensionParameter{
		{URI: sdp.SDESMidURI, ID: 20},
		{URI: testAudioLevelURI, ID: 2},
	}

	r := newResolvedRewriter(testPublisherExtensions(), subscriber, "7")

	h := testPublisherHeader(t)
	r.rewrite(&h)

	got := marshalRoundTrip(t, h)
	require.Equal(t, uint16(rtp.ExtensionProfileTwoByte), got.ExtensionProfile)
	require.Equal(t, []byte("7"), got.GetExtension(20))
	require.Equal(t, []byte{0x80}, got.GetExtension(2))
}

func TestHeaderExtensionRewriterDoesNotAllocate(t *testing.T) {
	subscriber := []webrtc.RTPHeaderExtensionParameter{
		{URI: sdp.SDESMidURI, ID: 9},
		{URI: testAudioLevelURI, ID: 2},
		{URI: testTransportCCURI, ID: 5},
	}

	r := newResolvedRewriter(testPublisherExtensions(), subscriber, "7")
	original := testPublisherHeader(t)

	allocs := testing.AllocsPerRun(1000, func() {
		h := original
		r.rewrite(&h)
	})

	require.Zero(t, allocs)
}

// midStamper sets the MID header extension on outgoing RTP the way browsers do. Pion doesn't, and the SFU
// forwarding the publisher's MID is the bug this guards against.
type midStamper struct {
	mu   sync.RWMutex
	mids map[uint32][]byte
	// streams whose MID is stamped as {mid}, like Firefox's mids, so it can't equal a subscriber's numeric mid
	braced map[string]bool
}

func newMidStamper(bracedStreamIDs ...string) *midStamper {
	braced := make(map[string]bool)
	for _, streamID := range bracedStreamIDs {
		braced[streamID] = true
	}

	return &midStamper{mids: make(map[uint32][]byte), braced: braced}
}

func (s *midStamper) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return &midStamperInterceptor{stamper: s}, nil
}

func (s *midStamper) mid(ssrc uint32) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.mids[ssrc]
}

// refresh reads the mids of pc's senders. It runs outside the write path, because PeerConnection
// methods there could block on a negotiation that is binding the same track.
func (s *midStamper) refresh(pc *webrtc.PeerConnection) {
	for _, transceiver := range pc.GetTransceivers() {
		sender := transceiver.Sender()
		if sender == nil || transceiver.Mid() == "" {
			continue
		}

		mid := []byte(transceiver.Mid())
		if track := sender.Track(); track != nil && s.braced[track.StreamID()] {
			mid = []byte("{" + transceiver.Mid() + "}")
		}

		s.mu.Lock()
		for _, encoding := range sender.GetParameters().Encodings {
			s.mids[uint32(encoding.SSRC)] = mid
			if encoding.RTX.SSRC != 0 {
				s.mids[uint32(encoding.RTX.SSRC)] = mid
			}
		}
		s.mu.Unlock()
	}
}

func (s *midStamper) run(ctx context.Context, pc *webrtc.PeerConnection) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(pc)
		}
	}
}

type midStamperInterceptor struct {
	interceptor.NoOp
	stamper *midStamper
}

func (i *midStamperInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var midID uint8

	for _, e := range info.RTPHeaderExtensions {
		if e.URI == sdp.SDESMidURI {
			midID = uint8(e.ID)
		}
	}

	if midID == 0 {
		return writer
	}

	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		if mid := i.stamper.mid(h.SSRC); mid != nil {
			_ = h.SetExtension(midID, mid)
		}

		return writer.Write(h, payload, a)
	})
}

// receivedMids records the MID and RID header extensions of the RTP a subscriber receives on one track.
type receivedMids struct {
	mu       sync.Mutex
	streamID string
	kind     webrtc.RTPCodecType
	mid      string
	packets  int
	withMid  int
	wrongMid []string
	withRID  int
}

func (r *receivedMids) snapshot() (packets, withMid int, wrongMid []string, withRID int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.packets, r.withMid, append([]string(nil), r.wrongMid...), r.withRID
}

func readReceivedMids(ctx context.Context, pc *webrtc.PeerConnection, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, record *receivedMids) {
	var midID, ridID, rridID uint8

	for _, e := range receiver.GetParameters().HeaderExtensions {
		switch e.URI {
		case sdp.SDESMidURI:
			midID = uint8(e.ID)
		case sdp.SDESRTPStreamIDURI:
			ridID = uint8(e.ID)
		case sdp.SDESRepairRTPStreamIDURI:
			rridID = uint8(e.ID)
		}
	}

	for _, transceiver := range pc.GetTransceivers() {
		if transceiver.Receiver() == receiver {
			record.mu.Lock()
			record.mid = transceiver.Mid()
			record.mu.Unlock()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := track.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return
		}

		p, _, err := track.ReadRTP()
		if errors.Is(err, io.EOF) {
			return
		}

		if err != nil {
			// read deadline
			continue
		}

		record.mu.Lock()
		record.packets++

		if midID != 0 {
			if mid := p.Header.GetExtension(midID); mid != nil {
				record.withMid++
				if string(mid) != record.mid {
					record.wrongMid = append(record.wrongMid, string(mid))
				}
			}
		}

		if (ridID != 0 && p.Header.GetExtension(ridID) != nil) || (rridID != 0 && p.Header.GetExtension(rridID) != nil) {
			record.withRID++
		}
		record.mu.Unlock()
	}
}

// TestForwardedMidAfterScreenShare reproduces a browser publisher (which stamps MID on its RTP) adding a screen
// share after a subscriber is connected. The subscriber must receive the screen track with its own mid in the
// MID extension, or none, never the publisher's.
func TestForwardedMidAfterScreenShare(t *testing.T) {
	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	roomManager := NewManager(ctx, "test", sfuOpts)
	defer roomManager.Close()

	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = &[]string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomManager.CreateRoomID(), "test-room", RoomTypeLocal, roomOpts)
	require.NoError(t, err)

	defer testRoom.Close()

	const screenStreamID = "screen"

	stamper := newMidStamper(screenStreamID)

	publisher, publisherClient, _, _ := createPeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "publisher", true, false, true, stamper)
	go stamper.run(ctx, publisher.PeerConnection)

	// CreatePeerPair's negotiate drops a negotiation that collides with one the SFU started, so the
	// publisher's negotiations are serialized here and retried until its tracks are negotiated
	var negotiateMu sync.Mutex

	renegotiate := func() {
		if !negotiateMu.TryLock() {
			return
		}

		go func() {
			defer negotiateMu.Unlock()

			negotiate(publisher.PeerConnection, publisherClient, TestLogger, true)
		}()
	}

	publisher.PeerConnection.OnNegotiationNeeded(renegotiate)
	publisherClient.OnAllowedRemoteRenegotiation(renegotiate)

	publisherMid := func(track webrtc.TrackLocal) string {
		for _, transceiver := range publisher.PeerConnection.GetTransceivers() {
			if transceiver.Sender() != nil && transceiver.Sender().Track() == track {
				return transceiver.Mid()
			}
		}

		return ""
	}

	waitNegotiated := func(name string, mid func() string) {
		lastAttempt := time.Now()

		require.Eventually(t, func() bool {
			mid := mid()
			remote := publisher.PeerConnection.CurrentRemoteDescription()

			if mid != "" && remote != nil && strings.Contains(remote.SDP, "a=mid:"+mid+"\r\n") &&
				publisher.PeerConnection.SignalingState() == webrtc.SignalingStateStable {
				return true
			}

			if time.Since(lastAttempt) > time.Second {
				lastAttempt = time.Now()
				renegotiate()
			}

			return false
		}, 30*time.Second, 50*time.Millisecond, "%s wasn't negotiated", name)
	}

	publish := func(track *webrtc.TrackLocalStaticSample) {
		SetPeerConnectionTracks(ctx, publisher.PeerConnection, []*webrtc.TrackLocalStaticSample{track})

		waitNegotiated("track of stream "+track.StreamID(), func() string { return publisherMid(track) })
	}

	publisherClient.OnTracksAdded(func(addedTracks []ITrack) {
		setTracks := make(map[string]TrackType)

		for _, track := range addedTracks {
			if track.StreamID() == screenStreamID {
				setTracks[track.ID()] = TrackTypeScreen
			} else {
				setTracks[track.ID()] = TrackTypeMedia
			}
		}

		publisherClient.SetTracksSourceType(setTracks)
	})

	subscriber, _, _, _ := CreatePeerPair(ctx, TestLogger, testRoom, DefaultTestIceServers(), "subscriber", true, false, true)

	var recordsMu sync.Mutex
	records := make([]*receivedMids, 0)

	subscriber.PeerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		record := &receivedMids{streamID: track.StreamID(), kind: track.Kind()}

		recordsMu.Lock()
		records = append(records, record)
		recordsMu.Unlock()

		go readReceivedMids(ctx, subscriber.PeerConnection, track, receiver, record)
	})

	describe := func() string {
		var b strings.Builder

		for name, pc := range map[string]*webrtc.PeerConnection{"publisher": publisher.PeerConnection, "subscriber": subscriber.PeerConnection} {
			fmt.Fprintf(&b, "%s (%s):", name, pc.SignalingState())

			for _, transceiver := range pc.GetTransceivers() {
				stream := ""
				if transceiver.Sender() != nil && transceiver.Sender().Track() != nil {
					stream = "send " + transceiver.Sender().Track().StreamID()
				}

				fmt.Fprintf(&b, " [mid %s %s %s]", transceiver.Mid(), transceiver.Kind(), stream)
			}

			b.WriteString("\n")
		}

		recordsMu.Lock()
		for _, record := range records {
			packets, withMid, _, _ := record.snapshot()
			fmt.Fprintf(&b, "subscriber received %s %s: %d packets, %d with mid\n", record.streamID, record.kind, packets, withMid)
		}
		recordsMu.Unlock()

		return b.String()
	}

	findVideo := func(streamID string) *receivedMids {
		recordsMu.Lock()
		defer recordsMu.Unlock()

		for _, record := range records {
			if record.streamID == streamID && record.kind == webrtc.RTPCodecTypeVideo {
				return record
			}
		}

		return nil
	}

	waitForVideo := func(streamID string) *receivedMids {
		var record *receivedMids

		ok := assert.Eventually(t, func() bool {
			record = findVideo(streamID)
			if record == nil {
				return false
			}

			packets, withMid, _, _ := record.snapshot()

			return packets >= 30 && withMid > 0
		}, 30*time.Second, 50*time.Millisecond, "subscriber didn't receive video of stream %s", streamID)

		require.True(t, ok, describe())

		return record
	}

	camera := waitForVideo("publisher")

	// the publisher starts a screen share after the subscriber is connected
	screenTrack, _ := GetStaticVideoTrack(ctx, ctx, GenerateSecureToken(), screenStreamID, true, "")
	publish(screenTrack)

	screen := waitForVideo(screenStreamID)

	t.Logf("publisher screen mid %s stamped as {%s}", publisherMid(screenTrack), publisherMid(screenTrack))

	// the screen's publisher MID can never equal the subscriber's, the camera's only may
	for _, check := range []struct {
		name   string
		record *receivedMids
	}{{"screen", screen}, {"camera", camera}} {
		packets, withMid, wrongMid, withRID := check.record.snapshot()

		check.record.mu.Lock()
		mid := check.record.mid
		check.record.mu.Unlock()

		t.Logf("%s: subscriber mid %s, %d packets, %d with mid", check.name, mid, packets, withMid)

		if len(wrongMid) > 0 {
			t.Fatalf("%s: %d of %d forwarded packets carry mid %q instead of the subscriber's %q", check.name, len(wrongMid), packets, wrongMid[0], mid)
		}

		require.Zero(t, withRID, "%s: forwarded packets carry a RID", check.name)
	}
}
