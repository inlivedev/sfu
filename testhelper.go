package sfu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/ice/v3"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/samespace/sfu/pkg/interceptors/simulcast"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
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

type PC struct {
	Mu                *sync.Mutex
	PeerConnection    *webrtc.PeerConnection
	IsInRenegotiation bool
}

func GetStaticTracks(ctx, iceConnectedCtx context.Context, streamID string, loop bool) ([]*webrtc.TrackLocalStaticSample, chan bool) {
	audioTrackID := GenerateSecureToken()
	videoTrackID := GenerateSecureToken()

	staticTracks := make([]*webrtc.TrackLocalStaticSample, 0)
	audioTrack, audioDoneChan := GetStaticAudioTrack(ctx, iceConnectedCtx, audioTrackID, streamID, loop)
	staticTracks = append(staticTracks, audioTrack)
	videoTrack, videoDoneChan := GetStaticVideoTrack(ctx, iceConnectedCtx, videoTrackID, streamID, loop, "low")
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
				select {
				case allDone <- true:
					return
				case <-ctxx.Done():
					return
				}
			}
		}
	}()

	return staticTracks, allDone
}

func GetStaticVideoTrack(ctx, iceConnectedCtx context.Context, trackID, streamID string, loop bool, quality string) (*webrtc.TrackLocalStaticSample, chan bool) {
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
		<-iceConnectedCtx.Done()
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

					h264Err = videoTrack.WriteSample(media.Sample{Data: nal.Data, Duration: h264FrameDuration})
					if h264Err != nil {
						panic(h264Err)
					}

				}
			}
		}
	}()

	return videoTrack, done
}

func GetStaticAudioTrack(ctx, iceConnectedCtx context.Context, trackID, streamID string, loop bool) (*webrtc.TrackLocalStaticSample, chan bool) {
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
		<-iceConnectedCtx.Done()
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

			defer func() {
				if rtpTranscv != nil {
					_ = rtpTranscv.Stop()
				}
			}()

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

func AddSimulcastVideoTracks(ctx, iceConnectedCtx context.Context, pc *webrtc.PeerConnection, trackID, streamID string) error {
	videoHigh, _ := GetStaticVideoTrack(ctx, iceConnectedCtx, trackID, streamID, true, "high")
	transcv, err := pc.AddTransceiverFromTrack(videoHigh, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		return err
	}

	videoMid, _ := GetStaticVideoTrack(ctx, iceConnectedCtx, trackID, streamID, true, "mid")
	if err = transcv.Sender().AddEncoding(videoMid); err != nil {
		return err
	}

	videoLow, _ := GetStaticVideoTrack(ctx, iceConnectedCtx, trackID, streamID, true, "low")
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

func CreatePeerPair(ctx context.Context, log logging.LeveledLogger, room *Room, iceServers []webrtc.ICEServer, peerName string, loop, isSimulcast bool) (*PC, *Client, stats.Getter, chan bool) {
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

	settingEngine := &webrtc.SettingEngine{}
	settingEngine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settingEngine.SetIncludeLoopbackCandidate(true)
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i))

	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			log.Infof("test: peer connection %s stated changed %s", peerName, state)
			if client != nil {
				_ = room.StopClient(client.ID())
			}
			cancelClient()
		}
	})

	peer := &PC{
		Mu:                &sync.Mutex{},
		PeerConnection:    pc,
		IsInRenegotiation: false,
	}

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(ctx)

	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		if connectionState == webrtc.ICEConnectionStateConnected {
			iceConnectedCtxCancel()
		}
	})

	if isSimulcast {
		_ = AddSimulcastVideoTracks(ctx, iceConnectedCtx, pc, GenerateSecureToken(), peerName)

		for _, sender := range pc.GetSenders() {
			parameters := sender.GetParameters()
			if parameters.Encodings[0].RID != "" {
				simulcastI.SetSenderParameters(parameters)
			}
		}

	} else {
		tracks, done = GetStaticTracks(clientContext, iceConnectedCtx, peerName, loop)
		SetPeerConnectionTracks(clientContext, pc, tracks)
	}

	allDone := make(chan bool)

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
					err := track.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					if err != nil {
						log.Errorf("set read deadline error:%v", err)
						return
					}
					_, _, err = track.Read(rtpBuff)
					if err == io.EOF {
						return
					}
				}

			}
		}()
	})

	go func() {
		ctxx, cancell := context.WithCancel(clientContext)
		defer cancell()

		for {
			select {
			case <-ctxx.Done():
				return

			case <-done:
				allDone <- true
				return
			}
		}
	}()

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	id := room.CreateClientID()
	client, _ = room.AddClient(id, id, DefaultClientOptions())

	client.OnTracksAdded(func(addedTracks []ITrack) {
		setTracks := make(map[string]TrackType, 0)
		for _, track := range addedTracks {
			setTracks[track.ID()] = TrackTypeMedia
		}
		client.SetTracksSourceType(setTracks)
	})

	client.OnTracksAvailable(func(availableTracks []ITrack) {
		subTracks := make([]SubscribeTrackRequest, 0)

		for _, t := range availableTracks {
			subTracks = append(subTracks, SubscribeTrackRequest{
				ClientID: t.ClientID(),
				TrackID:  t.ID(),
			})
		}

		_ = client.SubscribeTracks(subTracks)
	})

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.state.Load() == ClientStateEnded {
			log.Infof("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		currentTranscv := len(pc.GetTransceivers())

		log.Infof("test: got renegotiation %s", peerName)
		defer log.Infof("test: renegotiation done %s", peerName)
		if err = pc.SetRemoteDescription(offer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)

		if err = pc.SetLocalDescription(answer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		newTcv := len(pc.GetTransceivers()) - currentTranscv
		log.Infof("test: new transceiver %d total tscv %d", newTcv, len(pc.GetTransceivers()))

		peer.Mu.Lock()
		peer.IsInRenegotiation = false
		peer.Mu.Unlock()

		return *pc.LocalDescription(), nil
	})

	client.OnAllowedRemoteRenegotiation(func() {
		log.Infof("allowed remote renegotiation")
		negotiate(pc, client, log)
	})

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	negotiate(pc, client, log)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		err = client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
	})

	return peer, client, statsGetter, allDone
}

func negotiate(pc *webrtc.PeerConnection, client *Client, log logging.LeveledLogger) {
	if pc.SignalingState() != webrtc.SignalingStateStable {
		log.Infof("test: signaling state is not stable, skip renegotiation")
		return
	}

	if !client.IsAllowNegotiation() {
		log.Infof("test: client is not allowed to renegotiate")
		return
	}

	offer, _ := pc.CreateOffer(nil)

	_ = pc.SetLocalDescription(offer)

	answer, _ := client.Negotiate(offer)
	if answer != nil {
		_ = pc.SetRemoteDescription(*answer)
	}
}

func CreateDataPair(ctx context.Context, log logging.LeveledLogger, room *Room, iceServers []webrtc.ICEServer, peerName string, onDataChannel func(d *webrtc.DataChannel)) (*webrtc.PeerConnection, *Client, stats.Getter, chan webrtc.PeerConnectionState) {
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

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settingEngine.SetIncludeLoopbackCandidate(true)

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(settingEngine))

	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	pc.OnDataChannel(onDataChannel)

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		panic(err)
	}

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		panic(err)
	}

	connChan := make(chan webrtc.PeerConnectionState)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			log.Infof("test: peer connection closed ", peerName)
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
		log.Infof("allowed remote renegotiation")
		go negotiate(pc, client, log)
	})

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.state.Load() == ClientStateEnded {
			log.Infof("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		log.Infof("test: got renegotiation ", peerName)
		defer log.Infof("test: renegotiation done ", peerName)
		_ = pc.SetRemoteDescription(offer)
		answer, _ = pc.CreateAnswer(nil)
		_ = pc.SetLocalDescription(answer)
		return *pc.LocalDescription(), nil
	})

	negotiate(pc, client, log)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		err = client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
	})

	return pc, client, statsGetter, connChan
}

func LoadVp9Track(ctx context.Context, log logging.LeveledLogger, pc *webrtc.PeerConnection, videoFileName string, loop bool) *webrtc.RTPSender {
	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(ctx)

	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Infof("Connection State has changed %s \n", connectionState.String())
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

	transcv, videoTrackErr := pc.AddTransceiverFromTrack(videoTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	})

	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	ctxx, cancell := context.WithCancel(ctx)
	defer cancell()

	sender := transcv.Sender()

	go func() {
		rtcpBuf := make([]byte, 1500)

		for {
			select {
			case <-ctxx.Done():
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
		log.Infof("Connection established, start sending track: %s \n", header.FourCC)

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
					log.Infof("All video frames parsed and sent")
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
