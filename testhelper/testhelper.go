package testhelper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path"
	"runtime"
	"time"

	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/codec/x264"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"

	//nolint:blank-imports // Importing drivers
	_ "github.com/pion/mediadevices/pkg/driver/audiotest"
	_ "github.com/pion/mediadevices/pkg/driver/videotest"

	"github.com/pion/webrtc/v3/pkg/media/h264reader"

	"github.com/pion/webrtc/v3/pkg/media/oggreader"
)

var mediaEngine *webrtc.MediaEngine

const (
	videoFileName     = "./media/output.h264"
	audioFileName     = "./media/output.ogg"
	oggPageDuration   = time.Millisecond * 20
	h264FrameDuration = time.Millisecond * 33
)

func GetTestTracks() ([]mediadevices.Track, *webrtc.MediaEngine) {
	x264Params, _ := x264.NewParams()

	x264Params.BitRate = 500_000 // 500kbps

	opusParams, _ := opus.NewParams()

	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithVideoEncoders(&x264Params),
		mediadevices.WithAudioEncoders(&opusParams),
	)

	mediaEngine = &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	codecSelector.Populate(mediaEngine)

	s, _ := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(c *mediadevices.MediaTrackConstraints) {
			c.FrameFormat = prop.FrameFormat(frame.FormatI420)
			c.Width = prop.Int(640)
			c.Height = prop.Int(480)
		},
		Audio: func(c *mediadevices.MediaTrackConstraints) {
		},
		Codec: codecSelector,
	})

	return s.GetTracks(), mediaEngine
}

func GetStaticTracks(ctx context.Context, streamID string, loop bool) ([]*webrtc.TrackLocalStaticSample, *webrtc.MediaEngine, chan bool) {
	x264Params, _ := x264.NewParams()

	opusParams, _ := opus.NewParams()

	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithVideoEncoders(&x264Params),
		mediadevices.WithAudioEncoders(&opusParams),
	)

	mediaEngine = &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	audioTrackID := GenerateSecureToken(16)
	videoTrackID := GenerateSecureToken(16)

	codecSelector.Populate(mediaEngine)

	staticTracks := make([]*webrtc.TrackLocalStaticSample, 0)
	audioTrack, audioDoneChan := GetStaticAudioTrack(ctx, audioTrackID, streamID, loop)
	staticTracks = append(staticTracks, audioTrack)
	videoTrack, videoDoneChan := GetStaticVideoTrack(ctx, videoTrackID, streamID, loop)
	staticTracks = append(staticTracks, videoTrack)

	allDone := make(chan bool)

	go func() {
		trackDone := 0
		ctxx, cancel := context.WithCancel(ctx)
		defer cancel()

		for {
			select {
			case <-ctxx.Done():
				return
			case <-audioDoneChan:
				trackDone++
			case <-videoDoneChan:
				trackDone++
			}

			if trackDone == 2 && !loop {
				allDone <- true
			}
		}
	}()

	return staticTracks, mediaEngine, allDone
}

func GetVideoTrack() (mediadevices.Track, *webrtc.MediaEngine) {
	x264Params, _ := x264.NewParams()

	x264Params.BitRate = 500_000 // 500kbps

	opusParams, _ := opus.NewParams()

	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithVideoEncoders(&x264Params),
		mediadevices.WithAudioEncoders(&opusParams),
	)

	mediaEngine := webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	codecSelector.Populate(&mediaEngine)

	s, _ := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(c *mediadevices.MediaTrackConstraints) {
			c.FrameFormat = prop.FrameFormat(frame.FormatI420)
			c.Width = prop.Int(640)
			c.Height = prop.Int(480)
		},
		Codec: codecSelector,
	})

	return s.GetTracks()[0], &mediaEngine
}

func GetStaticVideoTrack(ctx context.Context, trackID, streamID string, loop bool) (*webrtc.TrackLocalStaticSample, chan bool) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("No caller information")
	}

	videoFileName := path.Join(path.Dir(filename), videoFileName)
	_, err := os.Stat(videoFileName)
	haveVideoFile := !os.IsNotExist(err)

	if !haveVideoFile {
		panic("no video file")
	}

	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, trackID, streamID)
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	done := make(chan bool)

	go func() {
		// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
		// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
		//
		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(h264FrameDuration)
		// this will loop
		for {
			// Open a H264 file and start reading using our IVFReader
			file, h264Err := os.Open(videoFileName)
			if h264Err != nil {
				panic(h264Err)
			}

			h264, h264Err := h264reader.NewReader(file)
			if h264Err != nil {
				panic(h264Err)
			}
		Loop:
			for ; true; <-ticker.C {
				select {
				case <-ctx.Done():
					return
				default:
					nal, h264Err := h264.NextNAL()
					if h264Err == io.EOF {
						if loop {
							break Loop
						} else {
							done <- true
							return
						}
					}
					if h264Err != nil {
						panic(h264Err)
					}

					if h264Err = videoTrack.WriteSample(media.Sample{Data: nal.Data, Duration: h264FrameDuration}); h264Err != nil {
						continue
					}
				}
			}
		}
	}()

	return videoTrack, done
}

func GetStaticAudioTrack(ctx context.Context, trackID, streamID string, loop bool) (*webrtc.TrackLocalStaticSample, chan bool) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("No caller information")
	}

	audioFileName := path.Join(path.Dir(filename), audioFileName)
	_, err := os.Stat(audioFileName)
	haveAudioFile := !os.IsNotExist(err)

	if !haveAudioFile {
		panic("no audio file")
	}

	// Create a audio track
	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, trackID, streamID)
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	done := make(chan bool)

	go func() {
		// Open a ogg file and start reading using our oggReader
		// Keep track of last granule, the difference is the amount of samples in the buffer
		var lastGranule uint64

		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(oggPageDuration)
		localCtx, cancel := context.WithCancel(ctx)

		defer cancel()

		for {
			// Open on oggfile in non-checksum mode.
			file, oggErr := os.Open(audioFileName)
			if oggErr != nil {
				panic(oggErr)
			}

			ogg, _, oggErr := oggreader.NewWith(file)
			if oggErr != nil {
				panic(oggErr)
			}
		Loop:
			for ; true; <-ticker.C {
				select {
				case <-localCtx.Done():
					return

				default:
					pageData, pageHeader, oggErr := ogg.ParseNextPage()
					if oggErr == io.EOF {
						if loop {
							break Loop
						} else {
							done <- true
							return
						}
					}

					if oggErr != nil {
						panic(oggErr)
					}

					// The amount of samples is the difference between the last and current timestamp
					sampleCount := float64(pageHeader.GranulePosition - lastGranule)
					lastGranule = pageHeader.GranulePosition
					sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

					if oggErr = audioTrack.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); oggErr != nil {
						continue
					}
				}
			}
		}
	}()

	return audioTrack, done
}

func SetPeerConnectionTracks(ctx context.Context, peerConnection *webrtc.PeerConnection, tracks []*webrtc.TrackLocalStaticSample) {
	for _, track := range tracks {
		rtpSender, trackErr := peerConnection.AddTrack(track)
		if trackErr != nil {
			panic(trackErr)
		}

		// Read incoming RTCP packets
		// Before these packets are returned they are processed by interceptors. For things
		// like NACK this needs to be called.
		go func() {
			rtcpBuf := make([]byte, 1500)
			ctxx, cancel := context.WithCancel(ctx)
			defer cancel()
			for {
				select {
				case <-ctxx.Done():
					return
				default:
					if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
						return
					}
				}

			}
		}()
	}
}

func GenerateSecureToken(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return ""
	}

	return hex.EncodeToString(b)
}
