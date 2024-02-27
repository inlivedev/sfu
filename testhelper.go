package sfu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"time"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/inlivedev/sfu/pkg/interceptors/simulcast"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"

	"github.com/pion/webrtc/v3/pkg/media/h264reader"
	"github.com/pion/webrtc/v3/pkg/media/ivfreader"

	"github.com/pion/webrtc/v3/pkg/media/oggreader"
)

var mediaEngine *webrtc.MediaEngine

const (
	videoFileName        = "./media/output-720p.h264"
	videoHalfFileName    = "./media/output-360p.h264"
	videoQuarterFileName = "./media/output-180p.h264"
	audioFileName        = "./media/output.ogg"
	oggPageDuration      = time.Millisecond * 20
	h264FrameDuration    = time.Millisecond * 33
)

func GetStaticTracks(ctx context.Context, streamID string, loop bool) ([]*webrtc.TrackLocalStaticSample, chan bool) {
	audioTrackID := GenerateSecureToken()
	videoTrackID := GenerateSecureToken()

	staticTracks := make([]*webrtc.TrackLocalStaticSample, 0)
	audioTrack, audioDoneChan := GetStaticAudioTrack(ctx, audioTrackID, streamID, loop)
	staticTracks = append(staticTracks, audioTrack)
	videoTrack, videoDoneChan := GetStaticVideoTrack(ctx, videoTrackID, streamID, loop, "low")
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

	return staticTracks, allDone
}

func GetStaticVideoTrack(ctx context.Context, trackID, streamID string, loop bool, quality string) (*webrtc.TrackLocalStaticSample, chan bool) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("No caller information")
	}

	var videoFile, rid string

	switch quality {
	case "mid":
		videoFile = videoHalfFileName
		rid = "mid"
	case "low":
		videoFile = videoQuarterFileName
		rid = "low"
	default:
		videoFile = videoFileName
		rid = "high"
	}

	videoFileName := path.Join(path.Dir(filename), videoFile)
	_, err := os.Stat(videoFileName)
	haveVideoFile := !os.IsNotExist(err)

	if !haveVideoFile {
		panic("no video file")
	}

	var videoTrack *webrtc.TrackLocalStaticSample

	if quality == "" {
		if videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, trackID, streamID); err != nil {
			panic(err)
		}
	} else {
		if videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, trackID, streamID, webrtc.WithRTPStreamID(rid)); err != nil {
			panic(err)
		}
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
		rtpTranscv, trackErr := peerConnection.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		})
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
					if _, _, rtcpErr := rtpTranscv.Sender().Read(rtcpBuf); rtcpErr != nil {
						return
					}
				}

			}
		}()
	}
}

func GenerateSecureToken() string {
	return uuid.New().String()
}

func AddSimulcastVideoTracks(ctx context.Context, pc *webrtc.PeerConnection, trackID, streamID string) error {
	videoHigh, _ := GetStaticVideoTrack(ctx, trackID, streamID, true, "high")
	transcv, err := pc.AddTransceiverFromTrack(videoHigh, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		return err
	}

	videoMid, _ := GetStaticVideoTrack(ctx, trackID, streamID, true, "mid")
	if err = transcv.Sender().AddEncoding(videoMid); err != nil {
		return err
	}

	videoLow, _ := GetStaticVideoTrack(ctx, trackID, streamID, true, "low")
	if err = transcv.Sender().AddEncoding(videoLow); err != nil {
		return err
	}

	// read outgoing packet to enable NACK
	go func() {
		rtcpBuf := make([]byte, 1500)
		localCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		for {
			select {
			case <-localCtx.Done():
				return
			default:
				if _, _, rtcpErr := transcv.Sender().Read(rtcpBuf); rtcpErr != nil {
					return
				}

			}
		}
	}()

	// parameters := sender.GetParameters()

	// var midID, ridID, rsidID uint8
	// for _, extension := range parameters.HeaderExtensions {
	// 	switch extension.URI {
	// 	case sdp.SDESMidURI:
	// 		midID = uint8(extension.ID)
	// 	case sdp.SDESRTPStreamIDURI:
	// 		ridID = uint8(extension.ID)
	// 	case SdesRepairRTPStreamIDURI:
	// 		rsidID = uint8(extension.ID)
	// 	}
	// }

	return nil
}

func GetMediaEngine() *webrtc.MediaEngine {
	mediaEngine = &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	return mediaEngine
}

func CreatePeerPair(ctx context.Context, room *Room, iceServers []webrtc.ICEServer, peerName string, loop, isSimulcast bool) (*webrtc.PeerConnection, *Client, stats.Getter, chan bool) {
	clientContext, cancelClient := context.WithCancel(ctx)
	var (
		client      *Client
		mediaEngine *webrtc.MediaEngine = GetMediaEngine()
		done        chan bool
		tracks      []*webrtc.TrackLocalStaticSample
	)

	i := &interceptor.Registry{}
	var simulcastI *simulcast.Interceptor

	if isSimulcast {
		simulcastFactory := simulcast.NewInterceptor()

		i.Add(simulcastFactory)

		simulcastFactory.OnNew(func(i *simulcast.Interceptor) {
			simulcastI = i
		})
	}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	var statsGetter stats.Getter

	statsInterceptorFactory.OnNewPeerConnection(func(_ string, g stats.Getter) {
		statsGetter = g
	})

	i.Add(statsInterceptorFactory)

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, i); err != nil {
		panic(err)
	}

	if isSimulcast {
		RegisterSimulcastHeaderExtensions(mediaEngine, webrtc.RTPCodecTypeVideo)
	}

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i))

	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	if isSimulcast {
		_ = AddSimulcastVideoTracks(ctx, pc, GenerateSecureToken(), peerName)

		for _, sender := range pc.GetSenders() {
			parameters := sender.GetParameters()
			if parameters.Encodings[0].RID != "" {
				simulcastI.SetSenderParameters(parameters)
			}
		}

	} else {
		tracks, done = GetStaticTracks(clientContext, peerName, loop)
		SetPeerConnectionTracks(ctx, pc, tracks)
	}

	allDone := make(chan bool)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			glog.Info("test: peer connection ", peerName, " stated changed ", state)
			if client != nil {
				_ = room.StopClient(client.ID())
				cancelClient()
			}
		}

	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		go func() {
			ctxx, cancell := context.WithCancel(clientContext)
			defer cancell()

			rtpBuff := make([]byte, 1500)
			for {
				select {
				case <-ctxx.Done():
					return
				default:
					_, _, err = track.Read(rtpBuff)
					if err == io.EOF {
						return
					}
				}

			}
		}()
	})

	go func() {

		for {
			select {
			case <-clientContext.Done():
				return

			case <-done:
				allDone <- true
			}
		}
	}()

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	id := room.CreateClientID()
	client, _ = room.AddClient(id, id, DefaultClientOptions())

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.state.Load() == ClientStateEnded {
			glog.Info("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		currentTranscv := len(pc.GetTransceivers())

		glog.Info("test: got renegotiation ", peerName)
		defer glog.Info("test: renegotiation done ", peerName)
		if err = pc.SetRemoteDescription(offer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)

		if err = pc.SetLocalDescription(answer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		newTcv := len(pc.GetTransceivers()) - currentTranscv
		glog.Info("test: new transceiver ", newTcv, " total tscv ", len(pc.GetTransceivers()))

		return *pc.LocalDescription(), nil
	})

	client.OnAllowedRemoteRenegotiation(func() {
		glog.Info("allowed remote renegotiation")
		negotiate(pc, client)
	})

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	negotiate(pc, client)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		err = client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
	})

	return pc, client, statsGetter, allDone
}

func negotiate(pc *webrtc.PeerConnection, client *Client) {
	if pc.SignalingState() != webrtc.SignalingStateStable {
		glog.Info("test: signaling state is not stable, skip renegotiation")
		return
	}

	if !client.IsAllowNegotiation() {
		glog.Info("test: client is not allowed to renegotiate")
		return
	}

	offer, _ := pc.CreateOffer(nil)

	_ = pc.SetLocalDescription(offer)
	answer, _ := client.Negotiate(offer)
	_ = pc.SetRemoteDescription(*answer)
}

func CreateDataPair(ctx context.Context, room *Room, iceServers []webrtc.ICEServer, peerName string, onDataChannel func(d *webrtc.DataChannel)) (*webrtc.PeerConnection, *Client, stats.Getter, chan webrtc.PeerConnectionState) {
	var (
		client      *Client
		mediaEngine *webrtc.MediaEngine = GetMediaEngine()
	)

	i := &interceptor.Registry{}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	var statsGetter stats.Getter

	statsInterceptorFactory.OnNewPeerConnection(func(_ string, g stats.Getter) {
		statsGetter = g
	})

	i.Add(statsInterceptorFactory)

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, i); err != nil {
		panic(err)
	}

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i))

	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	pc.OnDataChannel(onDataChannel)

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RtpTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		panic(err)
	}

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RtpTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		panic(err)
	}

	connChan := make(chan webrtc.PeerConnectionState)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			glog.Info("test: peer connection closed ", peerName)
			if client != nil {
				_ = room.StopClient(client.ID())
			}
		}

		connChan <- state

	})

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	id := room.CreateClientID()
	client, _ = room.AddClient(id, id, DefaultClientOptions())

	client.OnAllowedRemoteRenegotiation(func() {
		glog.Info("allowed remote renegotiation")
		go negotiate(pc, client)
	})

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.state.Load() == ClientStateEnded {
			glog.Info("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		glog.Info("test: got renegotiation ", peerName)
		defer glog.Info("test: renegotiation done ", peerName)
		_ = pc.SetRemoteDescription(offer)
		answer, _ = pc.CreateAnswer(nil)
		_ = pc.SetLocalDescription(answer)
		return *pc.LocalDescription(), nil
	})

	negotiate(pc, client)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		err = client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
	})

	return pc, client, statsGetter, connChan
}

func LoadVp9Track(ctx context.Context, pc *webrtc.PeerConnection, videoFileName string, loop bool) *webrtc.RTPSender {
	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(ctx)

	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		glog.Info("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			iceConnectedCtxCancel()
		}
	})

	file, openErr := os.Open(videoFileName)
	if openErr != nil {
		panic(openErr)
	}

	_, header, openErr := ivfreader.NewWith(file)
	if openErr != nil {
		panic(openErr)
	}

	// Determine video codec
	var trackCodec string
	switch header.FourCC {
	case "AV01":
		trackCodec = webrtc.MimeTypeAV1
	case "VP90":
		trackCodec = webrtc.MimeTypeVP9
	case "VP80":
		trackCodec = webrtc.MimeTypeVP8
	default:
		panic(fmt.Sprintf("Unable to handle FourCC %s", header.FourCC))
	}

	// Create a video track
	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: trackCodec}, "video", "pion")
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	transcv, videoTrackErr := pc.AddTransceiverFromTrack(videoTrack, webrtc.RtpTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	})

	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	ctxx, cancell := context.WithCancel(ctx)

	sender := transcv.Sender()

	go func() {
		rtcpBuf := make([]byte, 1500)

		for {
			select {
			case <-ctxx.Done():
				cancell()
				return
			default:
				if _, _, rtcpErr := sender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}
	}()

	go func() {
		ivf, header, ivfErr := openV9File(videoFileName)
		if ivfErr != nil {
			panic(ivfErr)
		}

		defer pc.Close()

		// Wait for connection established
		<-iceConnectedCtx.Done()
		glog.Info("Connection established, start sending track: %s \n", header.FourCC)

		// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
		// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
		//
		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(time.Millisecond * time.Duration((float32(header.TimebaseNumerator)/float32(header.TimebaseDenominator))*1000))
		for {
			select {
			case <-ctxx.Done():
				cancell()
				return
			case <-ticker.C:
				frame, _, ivfErr := ivf.ParseNextFrame()
				if errors.Is(ivfErr, io.EOF) {
					glog.Info("All video frames parsed and sent")
					if !loop {
						return
					} else {
						ivf, _, ivfErr = openV9File(videoFileName)
						if ivfErr != nil {
							panic(ivfErr)
						}
					}
				}

				if ivfErr = videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); ivfErr != nil {
					panic(ivfErr)
				}
			}
		}
	}()

	return sender
}

func openV9File(videoFileName string) (*ivfreader.IVFReader, *ivfreader.IVFFileHeader, error) {
	file, ivfErr := os.Open(videoFileName)
	if ivfErr != nil {
		return nil, nil, ivfErr
	}

	ivf, header, ivfErr := ivfreader.NewWith(file)
	if ivfErr != nil {
		return nil, nil, ivfErr
	}

	return ivf, header, nil
}
