package sfu

import (
	"context"
	"math"
	"testing"

	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// fakeScaleableTrack is an adjustable client track with fixed bitrates. Its quality level
// bitrates are derived from the receive bitrate, see bitrateClaim.QualityLevelToBitrate.
type fakeScaleableTrack struct {
	id             string
	sendBitrate    uint32
	receiveBitrate uint32
	maxQuality     QualityLevel
}

func (t *fakeScaleableTrack) push(*rtp.Packet, QualityLevel)          {}
func (t *fakeScaleableTrack) ID() string                              { return t.id }
func (t *fakeScaleableTrack) StreamID() string                        { return t.id }
func (t *fakeScaleableTrack) Context() context.Context                { return context.Background() }
func (t *fakeScaleableTrack) Kind() webrtc.RTPCodecType               { return webrtc.RTPCodecTypeVideo }
func (t *fakeScaleableTrack) MimeType() string                        { return webrtc.MimeTypeVP9 }
func (t *fakeScaleableTrack) LocalTrack() *webrtc.TrackLocalStaticRTP { return nil }
func (t *fakeScaleableTrack) IsScreen() bool                          { return false }
func (t *fakeScaleableTrack) IsSimulcast() bool                       { return false }
func (t *fakeScaleableTrack) IsScaleable() bool                       { return true }
func (t *fakeScaleableTrack) SetSourceType(TrackType)                 {}
func (t *fakeScaleableTrack) Client() *Client                         { return nil }
func (t *fakeScaleableTrack) RequestPLI()                             {}
func (t *fakeScaleableTrack) SetMaxQuality(quality QualityLevel)      { t.maxQuality = quality }
func (t *fakeScaleableTrack) MaxQuality() QualityLevel                { return t.maxQuality }
func (t *fakeScaleableTrack) ReceiveBitrate() uint32                  { return t.receiveBitrate }
func (t *fakeScaleableTrack) SendBitrate() uint32                     { return t.sendBitrate }
func (t *fakeScaleableTrack) Quality() QualityLevel                   { return QualityHigh }
func (t *fakeScaleableTrack) OnEnded(func())                          {}

func newTestBitrateController(track iClientTrack, quality QualityLevel) *bitrateController {
	bc := &bitrateController{
		enabledQualityLevels: DefaultQualityLevels(),
		log:                  logging.NewDefaultLoggerFactory().NewLogger("bitratecontroller"),
	}

	bc.claims.Store(track.ID(), &bitrateClaim{track: track, quality: quality})

	return bc
}

func TestBitrateDiffAndAdd(t *testing.T) {
	require.Equal(t, int64(-150_000), bitrateDiff(450_000, 300_000))
	require.Equal(t, int64(150_000), bitrateDiff(300_000, 450_000))

	require.Equal(t, uint32(300_000), addBitrate(450_000, -150_000))
	require.Equal(t, uint32(0), addBitrate(100_000, -150_000), "must clamp at 0 instead of wrapping around")
	require.Equal(t, uint32(math.MaxUint32), addBitrate(math.MaxUint32-1, 10))
}

// The measured send bitrate can be above the nominal bitrate of the next quality level,
// e.g. right after a layer switch. That must not wrap around and block the increase.
func TestFitBitratesToBandwidthIncreaseWhenSendBitrateAboveNextLevel(t *testing.T) {
	// nominal bitrates: lowlow 75k, lowmid 150k, low 300k, mid 600k, high 1.2M
	track := &fakeScaleableTrack{
		id:             "video",
		sendBitrate:    1_000_000,
		receiveBitrate: 1_200_000,
		maxQuality:     QualityHigh,
	}

	bc := newTestBitrateController(track, QualityLowLow)

	bc.fitBitratesToBandwidth(1_100_000)

	// high (1.2M) doesn't fit 1.1M, mid (600k) does
	require.Equal(t, QualityLevel(QualityMid), bc.GetClaim(track.ID()).Quality())
}

func TestFitBitratesToBandwidthDecreaseStopsWhenFit(t *testing.T) {
	track := &fakeScaleableTrack{
		id:             "video",
		sendBitrate:    1_000_000,
		receiveBitrate: 1_200_000,
		maxQuality:     QualityHigh,
	}

	bc := newTestBitrateController(track, QualityHigh)

	bc.fitBitratesToBandwidth(500_000)

	// mid (600k) doesn't fit 500k, low (300k) does
	require.Equal(t, QualityLevel(QualityLow), bc.GetClaim(track.ID()).Quality())
}

func TestIsEnoughBandwidthToIncreaseWhenSendBitrateAboveNextLevel(t *testing.T) {
	track := &fakeScaleableTrack{
		id:             "video",
		sendBitrate:    1_000_000,
		receiveBitrate: 1_200_000,
		maxQuality:     QualityHigh,
	}

	bc := newTestBitrateController(track, QualityLow)

	require.True(t, bc.isEnoughBandwidthToIncrase(0, bc.GetClaim(track.ID())))
}
