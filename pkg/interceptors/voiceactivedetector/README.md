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

	vadInterceptorFactory := voiceactivedetector.NewInterceptor(ctx)

	// enable voice detector
	vadInterceptorFactory.OnNew(func(i *voiceactivedetector.Interceptor) {
		vad = i
	})

	i.Add(vadInterceptorFactory)
	```

4. Use the interceptor registry to create PeerConnection
	```go
	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(i)).NewPeerConnection(peerConnectionConfig)
	```

5. Use the voice active detector  to detect voice activity on LocalStaticTrack
	```go
	// Create a new LocalStaticTrack
	localStaticTrack, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	detector := vad.AddAudioTrack(localStaticTrack)
	detector.OnVoiceDetected(func(activity voiceactivedetector.VoiceActivity) {
		// do something like sending it to client over datachanel or websocket
		
	})
	```