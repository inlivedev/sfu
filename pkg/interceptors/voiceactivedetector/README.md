# Voice Active Detector Interceptor
Voice Active Detector is a Pion Interceptor will allow you to detect any voice activity on the audio track that published to the client. It will provide you with the voice activity status and the audio level of the track.

## How to use

1. Import the package
	```go
	import "github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	```

2. Register the interceptor extension in the media engine when creating a PeerConnection 
	```go
	m := &webrtc.MediaEngine{}
	voiceactivedetector.RegisterAudioLevelHeaderExtension(m)
	```

3. Create a new VoiceActiveDetectorInterceptor
	```go
	var vad *voiceactivedetector.Interceptor

	i := &interceptor.Registry{}

	//"github.com/pion/logging"
	log:= logging.NewDefaultLoggerFactory().NewLogger("vad")

	// enable voice detector
	vadInterceptorFactory := voiceactivedetector.NewInterceptor(localCtx, log)

	vads := make(map[uint32]*voiceactivedetector.VoiceDetector)

	// enable voice detector
	vadInterceptorFactory.OnNew(func(i *voiceactivedetector.Interceptor) {
		vadInterceptor = i
		i.OnNewVAD(func(vad *voiceactivedetector.VoiceDetector) {
			vad.OnVoiceDetected(func(pkts []voiceactivedetector.VoicePacketData) {
				// add to vad map
				vads[vad.SSRC()] = vad
			})
		})
	})

		

	i.Add(vadInterceptorFactory)
	```

4. Use the interceptor registry to create PeerConnection
	```go
	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(i)).NewPeerConnection(peerConnectionConfig)
	```

5. Use the voice activity detector  to detect voice activity on remote track
	```go
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		vad, ok := vads[uint32(remoteTrack.SSRC())]
		if ok {
			vad.OnVoiceDetected(func(pkts []voiceactivedetector.VoicePacketData) {
				// voice detected on remote track
				voiceActivity := voiceactivedetector.VoiceActivity{
							TrackID:     remoteTrack.ID(),
							StreamID:    remoteTrack.StreamID(),
							SSRC:        uint32(remoteTrack.SSRC()),
							ClockRate:  remoteTrack.Codec().ClockRate,
							AudioLevels: pkts,
						}
				
				// do something with voice activity
				// send to datachannel or to user who subscribe to the event

			})
		}
	})
	```