package sfu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/interceptors/playoutdelay"
	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/inlivedev/sfu/pkg/pacer"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

type ClientState int
type ClientType string

const (
	ClientStateNew     = 0
	ClientStateActive  = 1
	ClientStateRestart = 2
	ClientStateEnded   = 3

	ClientTypePeer       = "peer"
	ClientTypeUpBridge   = "upbridge"
	ClientTypeDownBridge = "downbridge"

	QualityAudioRed = 5
	QualityAudio    = 4
	QualityHigh     = 3
	QualityMid      = 2
	QualityLow      = 1
	QualityNone     = 0

	messageTypeVideoSize  = "video_size"
	messageTypeStats      = "stats"
	messageTypeVADStarted = "vad_started"
	messageTypeVADEnded   = "vad_ended"
)

type QualityLevel uint32

var (
	ErrNegotiationIsNotRequested = errors.New("client: error negotiation is called before requested")
	ErrClientStoped              = errors.New("client: error client already stopped")
)

type ClientOptions struct {
	IdleTimeout          time.Duration
	Type                 string
	EnableVoiceDetection bool
	EnablePlayoutDelay   bool
	// Configure the minimum playout delay that will be used by the room
	// Recommendation:
	// 0 ms: Certain gaming scenarios (likely without audio) where we will want to play the frame as soon as possible. Also, for remote desktop without audio where rendering a frame asap makes sense
	// 100/150/200 ms: These could be the max target latency for interactive streaming use cases depending on the actual application (gaming, remoting with audio, interactive scenarios)
	// 400 ms: Application that want to ensure a network glitch has very little chance of causing a freeze can start with a minimum delay target that is high enough to deal with network issues. Video streaming is one example.
	MinPlayoutDelay uint16
	// Configure the minimum playout delay that will be used by the room
	// Recommendation:
	// 0 ms: Certain gaming scenarios (likely without audio) where we will want to play the frame as soon as possible. Also, for remote desktop without audio where rendering a frame asap makes sense
	// 100/150/200 ms: These could be the max target latency for interactive streaming use cases depending on the actual application (gaming, remoting with audio, interactive scenarios)
	// 400 ms: Application that want to ensure a network glitch has very little chance of causing a freeze can start with a minimum delay target that is high enough to deal with network issues. Video streaming is one example.
	MaxPlayoutDelay     uint16
	JitterBufferMinWait time.Duration
	JitterBufferMaxWait time.Duration
	// disable this will turn off the adaptive stream and jitter buffer. It just forward all received packets
	EnableAdaptiveStream bool
}

type internalDataMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type internalDataVAD struct {
	Type string                            `json:"type"`
	Data voiceactivedetector.VoiceActivity `json:"data"`
}

type internalDataStats struct {
	Type string            `json:"type"`
	Data remoteClientStats `json:"data"`
}

type internalDataVideoSize struct {
	Type string    `json:"type"`
	Data videoSize `json:"data"`
}

type videoSize struct {
	TrackID string `json:"track_id"`
	Width   uint32 `json:"width"`
	Height  uint32 `json:"height"`
}

type remoteClientStats struct {
	AvailableOutgoingBitrate uint64 `json:"available_outgoing_bitrate"`
	// this will be filled by the tracks stats
	// possible value are "cpu","bandwidth","both","none"
	QualityLimitationReason string `json:"quality_limitation_reason"`
}

type Client struct {
	id                    string
	name                  string
	bitrateController     *bitrateController
	context               context.Context
	cancel                context.CancelFunc
	canAddCandidate       *atomic.Bool
	clientTracks          map[string]iClientTrack
	internalDataChannel   *webrtc.DataChannel
	dataChannels          *DataChannelList
	estimator             cc.BandwidthEstimator
	initialReceiverCount  atomic.Uint32
	initialSenderCount    atomic.Uint32
	isInRenegotiation     *atomic.Bool
	isInRemoteNegotiation *atomic.Bool
	idleTimeoutContext    context.Context
	idleTimeoutCancel     context.CancelFunc
	mu                    sync.RWMutex
	peerConnection        *PeerConnection
	// pending received tracks are the remote tracks from other clients that waiting to add when the client is connected
	pendingReceivedTracks []SubscribeTrackRequest
	// pending published tracks are the remote tracks that still state as unknown source, and can't be published until the client state the source media or screen
	// the source can be set through client.SetTracksSourceType()
	pendingPublishedTracks *trackList
	// published tracks are the remote tracks from other clients that are published to this client
	publishedTracks                   *trackList
	pendingRemoteRenegotiation        *atomic.Bool
	queue                             *queue
	receiveRED                        bool
	state                             *atomic.Value
	sfu                               *SFU
	onConnectionStateChangedCallbacks []func(webrtc.PeerConnectionState)
	onJoinedCallbacks                 []func()
	onLeftCallbacks                   []func()
	onVoiceDetectedCallbacks          []func(voiceactivedetector.VoiceActivity)
	onTrackRemovedCallbacks           []func(sourceType string, track *webrtc.TrackLocalStaticRTP)
	onIceCandidate                    func(context.Context, *webrtc.ICECandidate)
	onBeforeRenegotiation             func(context.Context) bool
	onRenegotiation                   func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)
	onAllowedRemoteRenegotiation      func()
	onTracksAvailableCallbacks        []func([]ITrack)
	// onTrack is used by SFU to take action when a new track is added to the client
	onTrack                        func(ITrack)
	onTracksAdded                  func([]ITrack)
	options                        ClientOptions
	statsGetter                    stats.Getter
	stats                          *ClientStats
	tracks                         *trackList
	negotiationNeeded              *atomic.Bool
	pendingRemoteCandidates        []webrtc.ICECandidateInit
	pendingLocalCandidates         []*webrtc.ICECandidate
	quality                        *atomic.Uint32
	receivingBandwidth             *atomic.Uint32
	egressBandwidth                *atomic.Uint32
	ingressBandwidth               *atomic.Uint32
	ingressQualityLimitationReason *atomic.Value
	isDebug                        bool
	vad                            *voiceactivedetector.Interceptor
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		IdleTimeout:          5 * time.Minute,
		Type:                 ClientTypePeer,
		EnableVoiceDetection: false,
		EnablePlayoutDelay:   true,
		MinPlayoutDelay:      100,
		MaxPlayoutDelay:      200,
		JitterBufferMinWait:  20 * time.Millisecond,
		JitterBufferMaxWait:  150 * time.Millisecond,
		EnableAdaptiveStream: true,
	}
}

func NewClient(s *SFU, id string, name string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {
	var client *Client

	var vad *voiceactivedetector.Interceptor

	localCtx, cancel := context.WithCancel(s.context)
	m := &webrtc.MediaEngine{}

	if err := RegisterCodecs(m, s.codecs); err != nil {
		panic(err)
	}

	// let the client knows that we're receiving simulcast tracks
	RegisterSimulcastHeaderExtensions(m, webrtc.RTPCodecTypeVideo)

	if opts.EnableVoiceDetection {
		voiceactivedetector.RegisterAudioLevelHeaderExtension(m)
	}

	// // Create a InterceptorRegistry. This is the user configurable RTP/RTCP Pipeline.
	// // This provides NACKs, RTCP Reports and other features. If you use `webrtc.NewPeerConnection`
	// // this is enabled by default. If you are manually managing You MUST create a InterceptorRegistry
	// // for each PeerConnection.
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

	if opts.EnableVoiceDetection {
		glog.Info("client: voice detection is enabled")
		vadInterceptorFactory := voiceactivedetector.NewInterceptor(localCtx)

		// enable voice detector
		vadInterceptorFactory.OnNew(func(i *voiceactivedetector.Interceptor) {
			vad = i
		})

		i.Add(vadInterceptorFactory)
	}

	estimatorChan := make(chan cc.BandwidthEstimator, 1)

	// Create a Congestion Controller. This analyzes inbound and outbound data and provides
	// suggestions on how much we should be sending.
	//
	// Passing `nil` means we use the default Estimation Algorithm which is Google Congestion Control.
	// You can use the other ones that Pion provides, or write your own!
	congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		// if bw below 100_000, somehow the estimator will struggle to probe the bandwidth and will stuck there. So we set the min to 100_000
		// TODO: we need to use packet loss based bandwidth adjuster when the bandwidth is below 100_000
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(int(s.bitrateConfigs.InitialBandwidth)),
			gcc.SendSideBWEPacer(pacer.NewLeakyBucketPacer(int(s.bitrateConfigs.InitialBandwidth))),
			// gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
		)
	})
	if err != nil {
		panic(err)
	}

	congestionController.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
		estimatorChan <- estimator
	})

	i.Add(congestionController)

	if err = webrtc.ConfigureTWCCHeaderExtensionSender(m, i); err != nil {
		panic(err)
	}

	if opts.EnablePlayoutDelay {
		playoutdelay.RegisterPlayoutDelayHeaderExtension(m)
		playoutDelayInterceptor := playoutdelay.NewInterceptor(opts.MinPlayoutDelay, opts.MaxPlayoutDelay)

		i.Add(playoutDelayInterceptor)
	}

	// Use the default set of Interceptors
	if err := registerInterceptors(m, i); err != nil {
		panic(err)
	}

	settingEngine := webrtc.SettingEngine{}

	if s.mux != nil {
		settingEngine.SetICEUDPMux(s.mux.mux)
	} else {
		if err := settingEngine.SetEphemeralUDPPortRange(s.portStart, s.portEnd); err != nil {
			glog.Error("client: error set ephemeral udp port range ", err)
		}
	}

	if s.publicIP != "" {
		settingEngine.SetNAT1To1IPs([]string{s.publicIP}, s.nat1To1IPsCandidateType)
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(i)).NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}

	var stateNew atomic.Value
	stateNew.Store(ClientStateNew)

	var quality atomic.Uint32
	quality.Store(QualityHigh)
	client = &Client{
		id:                             id,
		name:                           name,
		context:                        localCtx,
		cancel:                         cancel,
		clientTracks:                   make(map[string]iClientTrack, 0),
		canAddCandidate:                &atomic.Bool{},
		isInRenegotiation:              &atomic.Bool{},
		isInRemoteNegotiation:          &atomic.Bool{},
		dataChannels:                   NewDataChannelList(localCtx),
		mu:                             sync.RWMutex{},
		negotiationNeeded:              &atomic.Bool{},
		peerConnection:                 newPeerConnection(peerConnection),
		state:                          &stateNew,
		tracks:                         newTrackList(),
		options:                        opts,
		pendingReceivedTracks:          make([]SubscribeTrackRequest, 0),
		pendingPublishedTracks:         newTrackList(),
		pendingRemoteRenegotiation:     &atomic.Bool{},
		publishedTracks:                newTrackList(),
		queue:                          NewQueue(localCtx),
		sfu:                            s,
		statsGetter:                    statsGetter,
		quality:                        &quality,
		receivingBandwidth:             &atomic.Uint32{},
		egressBandwidth:                &atomic.Uint32{},
		ingressBandwidth:               &atomic.Uint32{},
		ingressQualityLimitationReason: &atomic.Value{},
		onTracksAvailableCallbacks:     make([]func([]ITrack), 0),
		vad:                            vad,
	}

	// make sure the exisiting data channels is created on new clients
	s.createExistingDataChannels(client)

	var internalDataChannel *webrtc.DataChannel

	if internalDataChannel, err = client.createInternalDataChannel("internal", client.onInternalMessage); err != nil {
		glog.Error("client: error create internal data channel ", err)
	}

	client.internalDataChannel = internalDataChannel

	peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {

		glog.Info("client: connection state changed ", connectionState.String())

		client.onConnectionStateChanged(connectionState)

		switch connectionState {
		case webrtc.PeerConnectionStateConnected:
			if client.state.Load() == ClientStateNew {
				client.state.Store(ClientStateActive)
				client.onJoined()

				// trigger available tracks from other clients

				availableTracks := make([]ITrack, 0)

				for _, c := range s.clients.GetClients() {
					for _, track := range c.tracks.GetTracks() {
						_, err := client.publishedTracks.Get(track.ID())
						if track.ClientID() != client.ID() {
							if err == ErrTrackIsNotExists {
								availableTracks = append(availableTracks, track)
							} else {
								glog.Error("client: track already exists")
							}
						}
					}
				}

				// add relay tracks
				for _, track := range s.relayTracks {
					availableTracks = append(availableTracks, track)
				}

				if len(availableTracks) > 0 {
					glog.Info("client: ", client.ID(), " available tracks ", len(availableTracks))
					client.onTracksAvailable(availableTracks)
				}
			}

			if len(client.pendingReceivedTracks) > 0 {
				client.processPendingTracks()
				client.renegotiate()
			}

		case webrtc.PeerConnectionStateClosed:
			client.afterClosed()
		case webrtc.PeerConnectionStateFailed:
			client.startIdleTimeout(5 * time.Second)
		case webrtc.PeerConnectionStateConnecting:
			client.cancelIdleTimeout()
		case webrtc.PeerConnectionStateDisconnected:
			// do nothing it will idle failed or connected after a while
		case webrtc.PeerConnectionStateNew:
			// do nothing
			client.startIdleTimeout(opts.IdleTimeout)
		case webrtc.PeerConnectionState(webrtc.Unknown):
			// clean up
			client.afterClosed()
		}
	})

	// setup internal data channel
	if opts.EnableVoiceDetection {
		client.enableSendVADToInternalDataChannel()
		client.enableVADStatUpdate()
	}

	client.quality.Store(QualityHigh)

	client.ingressQualityLimitationReason.Store("none")

	client.stats = newClientStats(client)

	client.bitrateController = newbitrateController(client)

	go func() {
		estimator := <-estimatorChan
		client.mu.Lock()
		defer client.mu.Unlock()

		client.estimator = estimator

		client.bitrateController.MonitorBandwidth(estimator)
	}()

	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {

		var track ITrack

		remoteTrackID := strings.ReplaceAll(strings.ReplaceAll(remoteTrack.ID(), "{", ""), "}", "")

		defer glog.Info("client: ", client.ID(), " new track ", remoteTrackID, " Kind:", remoteTrack.Kind(), " Codec: ", remoteTrack.Codec().MimeType, " RID: ", remoteTrack.RID(), " Direction: ", receiver.RTPTransceiver().Direction())

		if remoteTrackID == "" {
			glog.Error("client: error remote track id is empty")
			return
		}

		onPLI := func() {
			if client.peerConnection == nil || client.peerConnection.PC() == nil || client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
				return
			}

			if err := client.peerConnection.PC().WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{MediaSSRC: uint32(remoteTrack.SSRC())},
			}); err != nil {
				glog.Error("client: error write pli ", err)
			}
		}

		onStatsUpdated := func(stats *stats.Stats) {
			client.mu.Lock()
			defer client.mu.Unlock()

			client.stats.SetReceiver(track.ID(), *stats)
		}

		if remoteTrack.RID() == "" {
			// not simulcast

			minWait := opts.JitterBufferMinWait
			maxWait := opts.JitterBufferMaxWait

			track = newTrack(client.context, client.id, remoteTrack, minWait, maxWait, s.pliInterval, onPLI, client.statsGetter, onStatsUpdated)

			go func() {
				ctx, cancel := context.WithCancel(track.Context())
				defer cancel()
				<-ctx.Done()
				client.stats.removeReceiverStats(remoteTrack.ID())
			}()

			if err := client.tracks.Add(track); err != nil {
				glog.Error("client: error add track ", err)
			}

			client.onTrack(track)
			track.SetAsProcessed()
		} else {
			// simulcast
			var simulcast *SimulcastTrack
			var ok bool

			id := remoteTrack.ID()

			track, err = client.tracks.Get(id) // not found because the track is not added yet due to race condition

			if err != nil {
				// if track not found, add it
				track = newSimulcastTrack(client.context, client.id, remoteTrack, opts.JitterBufferMinWait, opts.JitterBufferMaxWait, s.pliInterval, onPLI, client.statsGetter, onStatsUpdated)
				if err := client.tracks.Add(track); err != nil {
					glog.Error("client: error add track ", err)
				}

				go func() {
					ctx, cancel := context.WithCancel(track.Context())
					defer cancel()
					<-ctx.Done()
					client.stats.removeReceiverStats(remoteTrack.ID())
				}()

				if simulcast, ok = track.(*SimulcastTrack); !ok {
					glog.Error("client: error track is not simulcast track")
				}

			} else if simulcast, ok = track.(*SimulcastTrack); ok {
				simulcast.AddRemoteTrack(simulcast.context, remoteTrack, opts.JitterBufferMinWait, opts.JitterBufferMaxWait, client.statsGetter, onStatsUpdated, onPLI)
			}

			if !track.IsProcessed() {
				client.onTrack(track)
				track.SetAsProcessed()
			}

		}
	})

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		client.mu.Lock()
		defer client.mu.Unlock()

		// only sending candidate when the local description is set, means expecting the remote peer already has the remote description
		if candidate != nil {
			if client.canAddCandidate.Load() {
				go client.onIceCandidateCallback(candidate)

				return
			}

			client.pendingLocalCandidates = append(client.pendingLocalCandidates, candidate)
		}
	})

	return client
}

func (c *Client) ID() string {
	return c.id
}

func (c *Client) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.name
}

func (c *Client) Context() context.Context {
	return c.context
}

// OnTrackAdded event is to confirmed the source type of the pending published tracks.
// If the event is not listened, the pending published tracks will be ignored and not published to other clients.
// Once received, respond with `client.SetTracksSourceType()“ to confirm the source type of the pending published tracks
func (c *Client) OnTracksAdded(callback func(addedTracks []ITrack)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onTracksAdded = callback
}

// Init and Complete negotiation is used for bridging the room between servers
func (c *Client) InitNegotiation() *webrtc.SessionDescription {
	offer, err := c.peerConnection.PC().CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	err = c.peerConnection.PC().SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	// allow add candidates once the local description is set
	c.canAddCandidate.Store(true)

	return c.peerConnection.PC().LocalDescription()
}

func (c *Client) CompleteNegotiation(answer webrtc.SessionDescription) {
	err := c.peerConnection.PC().SetRemoteDescription(answer)
	if err != nil {
		panic(err)
	}
}

// ask if allowed for remote negotiation is required before call negotiation to make sure there is no racing condition of negotiation between local and remote clients.
// return false means the negotiation is in process, the requester must have a mechanism to repeat the request once it's done.
// requesting this must be followed by calling Negotate() to make sure the state is completed. Failed on called Negotiate() will cause the client to be in inconsistent state.
func (c *Client) IsAllowNegotiation() bool {
	if c.isInRenegotiation.Load() {
		c.pendingRemoteRenegotiation.Store(true)
		return false
	}

	c.isInRemoteNegotiation.Store(true)

	return true
}

// SDP negotiation from remote client
func (c *Client) Negotiate(offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	glog.Info("client: negotiation started ", c.ID)
	defer glog.Info("client: negotiation done ", c.ID)

	answerChan := make(chan webrtc.SessionDescription)
	errorChan := make(chan error)
	c.queue.Push(negotiationQueue{
		Client:     c,
		SDP:        offer,
		AnswerChan: answerChan,
		ErrorChan:  errorChan,
	})

	select {
	case err := <-errorChan:
		return nil, err
	case answer := <-answerChan:
		return &answer, nil
	}
}

func (c *Client) negotiateQueuOp(offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	c.isInRemoteNegotiation.Store(true)

	defer func() {
		c.isInRemoteNegotiation.Store(false)
	}()

	currentReceiversCount := 0
	currentSendersCount := 0
	for _, trscv := range c.peerConnection.PC().GetTransceivers() {
		if trscv.Receiver() != nil {
			currentReceiversCount++
		}

		if trscv.Sender() != nil {
			currentSendersCount++
		}
	}

	if !c.receiveRED {
		match, err := regexp.MatchString(`a=rtpmap:63`, offer.SDP)
		if err != nil {
			glog.Error("client: error on check RED support in SDP ", err)
		} else {
			c.receiveRED = match
		}
	}

	// Set the remote SessionDescription
	err := c.peerConnection.PC().SetRemoteDescription(offer)
	if err != nil {
		glog.Error("client: error set remote description ", err)

		return nil, err
	}

	// Create answer
	answer, err := c.peerConnection.PC().CreateAnswer(nil)
	if err != nil {
		glog.Error("client: error create answer ", err)
		return nil, err
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = c.peerConnection.PC().SetLocalDescription(answer)
	if err != nil {
		glog.Error("client: error set local description ", err)
		return nil, err
	}

	// allow add candidates once the local description is set
	c.canAddCandidate.Store(true)

	// process pending ice
	for _, iceCandidate := range c.pendingRemoteCandidates {
		err = c.peerConnection.PC().AddICECandidate(iceCandidate)
		if err != nil {
			glog.Error("client: error add ice candidate ", err)
			return nil, err
		}
	}

	newReceiversCount := 0
	newSenderCount := 0
	for _, trscv := range c.peerConnection.PC().GetTransceivers() {
		if trscv.Receiver() != nil {
			newReceiversCount++
		}

		if trscv.Sender() != nil {
			newSenderCount++
		}
	}

	initialReceiverCount := newReceiversCount - currentReceiversCount

	c.initialReceiverCount.Store(uint32(initialReceiverCount))

	initialSenderCount := newSenderCount - currentSendersCount

	c.initialSenderCount.Store(uint32(initialSenderCount))

	// send pending local candidates if any
	go c.sendPendingLocalCandidates()

	c.pendingRemoteCandidates = nil

	return c.peerConnection.PC().LocalDescription(), nil
}

func (c *Client) renegotiate() {
	c.queue.Push(renegotiateQueue{
		Client: c,
	})
}

// OnBeforeRenegotiation event is called before the SFU is trying to renegotiate with the client.
// The client must be listen for this event and set the callback to return true if the client is ready to renegotiate
// and no current negotiation is in progress from the client.
func (c *Client) OnBeforeRenegotiation(callback func(context.Context) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onBeforeRenegotiation = callback
}

// OnRenegotiation event is called when the SFU is trying to renegotiate with the client.
// The callback will receive the SDP offer from the SFU that must be use to create the SDP answer from the client.
// The SDP answer then can be passed back to the SFU using `client.CompleteNegotiation()` method.
func (c *Client) OnRenegotiation(callback func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onRenegotiation = callback
}

func (c *Client) renegotiateQueuOp() {
	c.mu.Lock()
	if c.onRenegotiation == nil {
		glog.Error("client: onRenegotiation is not set, can't do renegotiation")
		c.mu.Unlock()

		return
	}

	c.mu.Unlock()

	if c.isInRemoteNegotiation.Load() {
		glog.Info("sfu: renegotiation is delayed because the remote client is doing negotiation ", c.ID)

		return
	}

	c.negotiationNeeded.Store(true)

	// no need to run another negotiation if it's already in progress, it will rerun because we mark the negotiationneeded to true
	if c.isInRenegotiation.Load() {
		glog.Info("sfu: renegotiation is delayed because the client is doing negotiation ", c.ID)
		return
	}

	// mark negotiation is in progress to make sure no concurrent negotiation
	c.isInRenegotiation.Store(true)

	go func() {
		defer c.isInRenegotiation.Store(false)
		timout, cancel := context.WithTimeout(c.context, 100*time.Millisecond)
		defer cancel()

		<-timout.Done()

		for c.negotiationNeeded.Load() {
			ctxx, cancell := context.WithCancel(c.context)
			defer cancell()
			select {
			case <-ctxx.Done():
				return
			default:
				// mark negotiation is not needed after this done, so it will out of the loop
				c.negotiationNeeded.Store(false)

				// only renegotiate when client is connected
				if c.state.Load() != ClientStateEnded &&
					c.peerConnection.PC().SignalingState() == webrtc.SignalingStateStable &&
					c.peerConnection.PC().ConnectionState() == webrtc.PeerConnectionStateConnected {

					if c.onRenegotiation == nil {
						return
					}

					offer, err := c.peerConnection.PC().CreateOffer(nil)
					if err != nil {
						glog.Error("sfu: error create offer on renegotiation ", err)
						return
					}

					// Sets the LocalDescription, and starts our UDP listeners
					err = c.peerConnection.PC().SetLocalDescription(offer)
					if err != nil {
						glog.Error("sfu: error set local description on renegotiation ", err)
						return
					}

					// this will be blocking until the renegotiation is done
					answer, err := c.onRenegotiation(c.context, *c.peerConnection.PC().LocalDescription())
					if err != nil {
						//TODO: when this happen, we need to close the client and ask the remote client to reconnect
						glog.Error("sfu: error on renegotiation ", err)
						return
					}

					if answer.Type != webrtc.SDPTypeAnswer {
						glog.Error("sfu: error on renegotiation, the answer is not an answer type")
						return
					}

					err = c.peerConnection.PC().SetRemoteDescription(answer)
					if err != nil {
						return
					}
				}
			}
		}
	}()

}

func (c *Client) allowRemoteRenegotiation() {
	c.queue.Push(allowRemoteRenegotiationQueue{
		Client: c,
	})
}

// OnAllowedRemoteRenegotiation event is called when the SFU is done with the renegotiation
// and ready to receive the renegotiation from the client.
// Use this event to trigger the client to do renegotiation if needed.
func (c *Client) OnAllowedRemoteRenegotiation(callback func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onAllowedRemoteRenegotiation = callback
}

// inform to remote client that it's allowed to do renegotiation through event
func (c *Client) allowRemoteRenegotiationQueuOp() {
	if c.onAllowedRemoteRenegotiation != nil {
		c.isInRemoteNegotiation.Store(true)
		go c.onAllowedRemoteRenegotiation()
	}
}

func (c *Client) setClientTrack(t ITrack) iClientTrack {
	var outputTrack iClientTrack

	err := c.publishedTracks.Add(t)
	if err != nil {
		return nil
	}

	if t.IsSimulcast() {
		simulcastTrack := t.(*SimulcastTrack)
		outputTrack = simulcastTrack.subscribe(c)

	} else {
		singleTrack := t.(*Track)
		outputTrack = singleTrack.subscribe(c)
	}

	localTrack := outputTrack.LocalTrack()

	senderTcv, err := c.peerConnection.PC().AddTransceiverFromTrack(localTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		glog.Error("client: error on adding track ", err)
		return nil
	}

	go func() {
		ctx, cancel := context.WithCancel(outputTrack.Context())
		defer cancel()

		<-ctx.Done()

		if c == nil {
			return
		}

		defer func() {
			c.mu.Lock()
			delete(c.clientTracks, outputTrack.ID())
			c.mu.Unlock()

			c.renegotiate()
		}()

		sender := senderTcv.Sender()
		if sender == nil {
			return
		}

		if c.peerConnection == nil || c.peerConnection.PC() == nil || sender == nil || c.peerConnection.PC().ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}

		err = senderTcv.Stop()
		if err != nil {
			glog.Error("client: error stop sender ", err)
		}
	}()

	// enable RTCP report and stats
	c.enableReportAndStats(senderTcv.Sender(), outputTrack)

	c.mu.Lock()
	c.clientTracks[outputTrack.ID()] = outputTrack
	c.mu.Unlock()

	return outputTrack
}

func (c *Client) ClientTracks() map[string]iClientTrack {
	c.mu.RLock()
	defer c.mu.RUnlock()

	clientTracks := make(map[string]iClientTrack, 0)
	for k, v := range c.clientTracks {
		clientTracks[k] = v
	}

	return clientTracks
}

func (c *Client) enableReportAndStats(rtpSender *webrtc.RTPSender, track iClientTrack) {
	go func() {
		localCtx, cancel := context.WithCancel(track.Context())

		defer cancel()

		for {
			select {
			case <-localCtx.Done():
				return
			default:
				rtcpPackets, _, err := rtpSender.ReadRTCP()
				if err != nil && err == io.ErrClosedPipe {
					return
				}

				for _, p := range rtcpPackets {
					switch p.(type) {
					case *rtcp.PictureLossIndication:
						track.RequestPLI()
					case *rtcp.FullIntraRequest:
						track.RequestPLI()
					}
				}
			}
		}
	}()

	go func() {
		localCtx, cancel := context.WithCancel(track.Context())
		tick := time.NewTicker(1 * time.Second)
		defer tick.Stop()

		defer cancel()
		for {
			select {
			case <-localCtx.Done():
				return
			case <-tick.C:
				c.updateSenderStats(rtpSender)
			}
		}
	}()
}

func (c *Client) processPendingTracks() {
	if len(c.pendingReceivedTracks) > 0 {
		err := c.SubscribeTracks(c.pendingReceivedTracks)
		if err != nil {
			glog.Error("client: error subscribe tracks ", err)
		}

		c.pendingReceivedTracks = make([]SubscribeTrackRequest, 0)
	}
}

// make sure to call this when client's done to clean everything
func (c *Client) afterClosed() {
	state := c.state.Load()
	if state != ClientStateEnded {
		c.state.Store(ClientStateEnded)
	}

	c.internalDataChannel.Close()

	c.onLeft()

	c.cancel()

	c.sfu.onAfterClientStopped(c)
}

func (c *Client) stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.peerConnection.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return nil
	}

	err := c.peerConnection.Close()
	if err != nil {
		return err
	}

	return nil
}

// End will wait until the client is completely stopped
func (c *Client) End() {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := make(chan bool)

	c.sfu.OnClientRemoved(func(removedClient *Client) {
		if removedClient.ID() == c.ID() {
			removed <- true
		}
	})

	<-removed
}

func (c *Client) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if c.peerConnection.PC().RemoteDescription() == nil {
		c.pendingRemoteCandidates = append(c.pendingRemoteCandidates, candidate)
	} else {
		if err := c.peerConnection.PC().AddICECandidate(candidate); err != nil {
			glog.Error("client: error add ice candidate ", err)
			return err
		}
	}

	return nil
}

// OnTracksAvailable event is called when the SFU has ice candidate that need to pass to the client.
// This event will triggered during negotiation process to exchanges ice candidates between SFU and client.
// The client can also pass the ice candidate to the SFU using `client.AddICECandidate()` method.
func (c *Client) OnIceCandidate(callback func(context.Context, *webrtc.ICECandidate)) {
	c.onIceCandidate = callback
}

func (c *Client) onIceCandidateCallback(candidate *webrtc.ICECandidate) {
	if c.onIceCandidate == nil {
		glog.Info("client: on ice candidate callback is not set")
		return
	}

	c.onIceCandidate(c.context, candidate)
}

func (c *Client) sendPendingLocalCandidates() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, candidate := range c.pendingLocalCandidates {
		c.onIceCandidateCallback(candidate)
	}

	c.pendingLocalCandidates = nil
}

// OnConnectionStateChanged event is called when the SFU connection state is changed.
// The callback will receive the connection state as the new state.
func (c *Client) OnConnectionStateChanged(callback func(webrtc.PeerConnectionState)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onConnectionStateChangedCallbacks = append(c.onConnectionStateChangedCallbacks, callback)
}

func (c *Client) onConnectionStateChanged(state webrtc.PeerConnectionState) {
	c.mu.RLock()
	callbacks := c.onConnectionStateChangedCallbacks
	c.mu.RUnlock()
	for _, callback := range callbacks {
		go callback(webrtc.PeerConnectionState(state))
	}
}

func (c *Client) onJoined() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, callback := range c.onJoinedCallbacks {
		callback()
	}
}

// OnJoined event is called when the client is joined to the room.
// This doesn't mean that the client's tracks are already published to the room.
// This event can be use to track number of clients in the room.
func (c *Client) OnJoined(callback func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onJoinedCallbacks = append(c.onJoinedCallbacks, callback)
}

// OnLeft event is called when the client is left from the room.
// This event can be use to track number of clients in the room.
func (c *Client) OnLeft(callback func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onLeftCallbacks = append(c.onLeftCallbacks, callback)
}

func (c *Client) onLeft() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, callback := range c.onLeftCallbacks {
		go callback()
	}
}

// OnTrackRemoved event is called when the client's track is removed from the room.
// Usually this triggered when the client is disconnected from the room or a track is unpublished from the client.
func (c *Client) OnTrackRemoved(callback func(sourceType string, track *webrtc.TrackLocalStaticRTP)) {
	c.onTrackRemovedCallbacks = append(c.onTrackRemovedCallbacks, callback)
}

func (c *Client) IsBridge() bool {
	return c.Type() == ClientTypeUpBridge || c.Type() == ClientTypeDownBridge
}

func (c *Client) startIdleTimeout(timeout time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// cancel previous timeout and start a new one
	if c.idleTimeoutContext != nil && c.idleTimeoutContext.Err() == nil {
		c.idleTimeoutCancel()
	}

	c.idleTimeoutContext, c.idleTimeoutCancel = context.WithTimeout(c.context, timeout)

	go func() {
		<-c.idleTimeoutContext.Done()
		if c == nil || c.idleTimeoutContext == nil || c.idleTimeoutCancel == nil {
			return
		}

		defer c.idleTimeoutCancel()

		err := c.idleTimeoutContext.Err()
		if err != nil && err == context.DeadlineExceeded {
			glog.Info("client: idle timeout reached ", c.ID)
			c.afterClosed()
		}
	}()
}

func (c *Client) cancelIdleTimeout() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.idleTimeoutCancel != nil {
		c.idleTimeoutCancel()
		c.idleTimeoutContext = nil
		c.idleTimeoutCancel = nil
	}
}

func (c *Client) Type() string {
	return c.options.Type
}

func (c *Client) PeerConnection() *PeerConnection {
	return c.peerConnection
}

func (c *Client) Stats() *ClientStats {
	return c.stats
}

func (c *Client) updateSenderStats(sender *webrtc.RTPSender) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.statsGetter == nil {
		return
	}

	if sender == nil {
		return
	}

	if sender.Track() == nil {
		return
	}

	ssrc := sender.GetParameters().Encodings[0].SSRC

	stats := c.statsGetter.Get(uint32(ssrc))
	if stats != nil {
		c.stats.SetSender(sender.Track().ID(), *stats)
	}
}

// SetTracksSourceType set the source type of the pending published tracks.
// This function must be called after receiving OnTracksAdded event.
// The source type can be "media" or "screen"
// Calling this method will trigger `client.OnTracksAvailable` event to other clients.
// The other clients then can subscribe the tracks using `client.SubscribeTracks()` method.
func (c *Client) SetTracksSourceType(trackTypes map[string]TrackType) {
	availableTracks := make([]ITrack, 0)
	removeTrackIDs := make([]string, 0)
	for _, track := range c.pendingPublishedTracks.GetTracks() {
		if trackType, ok := trackTypes[track.ID()]; ok {
			track.SetSourceType(trackType)
			availableTracks = append(availableTracks, track)

			// remove it from pending published once it published available to other clients
			removeTrackIDs = append(removeTrackIDs, track.ID())
		}
	}

	c.pendingPublishedTracks.remove(removeTrackIDs)

	if len(availableTracks) > 0 {
		// broadcast to other clients available tracks from this client
		glog.Info("client: ", c.ID(), " set source tracks ", len(availableTracks))
		c.sfu.onTracksAvailable(c.ID(), availableTracks)
	}
}

// SubscribeTracks subscribe tracks from other clients that are published to this client
// The client must listen for `client.OnTracksAvailable` to know if a new track is available to subscribe.
// Calling subscribe tracks will trigger the SFU renegotiation with the client.
// Make sure you already listen for `client.OnRenegotiation()` before calling this method to track subscription can complete.
func (c *Client) SubscribeTracks(req []SubscribeTrackRequest) error {
	if c.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		c.pendingReceivedTracks = append(c.pendingReceivedTracks, req...)

		return nil
	}

	clientTracks := make([]iClientTrack, 0)

	for _, r := range req {
		trackFound := false

		// skip track if it's own track
		if c.ID() == r.ClientID {
			continue
		}

		if client, err := c.sfu.clients.GetClient(r.ClientID); err == nil {
			for _, track := range client.tracks.GetTracks() {
				if track.ID() == r.TrackID {
					if clientTrack := c.setClientTrack(track); clientTrack != nil {
						clientTracks = append(clientTracks, clientTrack)
					}

					glog.Info("client: subscribe track ", r.TrackID, " from ", r.ClientID, " to ", c.ID())

					trackFound = true

				}
			}

			// look on relay tracks
			for _, track := range c.SFU().relayTracks {
				if track.ID() == r.TrackID {
					if clientTrack := c.setClientTrack(track); clientTrack != nil {
						clientTracks = append(clientTracks, clientTrack)
					}

					trackFound = true
				}
			}
		} else if err != nil {
			return err
		}

		if !trackFound {
			return fmt.Errorf("track %s not found", r.TrackID)
		}
	}

	if len(clientTracks) > 0 {
		// claim bitrates
		if err := c.bitrateController.addClaims(clientTracks); err != nil {
			glog.Error("sfu: failed to add claims ", err)
		}

		c.renegotiate()

		// request keyframe
		for _, track := range clientTracks {
			track.RequestPLI()
		}
	}

	return nil
}

// SetQuality method is to set the maximum quality of the video that will be sent to the client.
// This is for bandwidth efficiency purpose and use when the video is rendered in smaller size than the original size.
func (c *Client) SetQuality(quality QualityLevel) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.quality.Load() == uint32(quality) {
		return
	}

	glog.Infof("client: %s switch quality to %s", c.ID, quality)
	c.quality.Store(uint32(quality))
	for _, claim := range c.bitrateController.Claims() {
		if claim.track.IsSimulcast() {
			claim.track.(*simulcastClientTrack).remoteTrack.sendPLI()
		} else if claim.track.IsScaleable() {
			claim.track.RequestPLI()
		}
	}
}

// GetEstimatedBandwidth returns the estimated bandwidth in bits per second based on
// Google Congestion Controller estimation. If the congestion controller is not enabled,
// it will return the initial bandwidth. If the receiving bandwidth is not 0, it will return the smallest value between
// the estimated bandwidth and the receiving bandwidth.
func (c *Client) GetEstimatedBandwidth() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()

	estimated := uint32(0)

	if c.estimator == nil {
		estimated = uint32(c.sfu.bitrateConfigs.InitialBandwidth)
	} else {
		estimated = uint32(c.estimator.GetTargetBitrate())
		c.egressBandwidth.Store(estimated)
	}

	receivingBandwidth := c.receivingBandwidth.Load()

	if receivingBandwidth != 0 && receivingBandwidth < estimated {
		return receivingBandwidth
	}

	return estimated
}

// This should get from the publisher client using RTCIceCandidatePairStats.availableOutgoingBitrate
// from client stats. It should be done through DataChannel so it won't required additional implementation on API endpoints
// where this SFU is used.
func (c *Client) UpdatePublisherBandwidth(bitrate uint32) {
	if bitrate == 0 {
		return
	}

	c.ingressBandwidth.Store(bitrate)
}

func (c *Client) createDataChannel(label string, initOpts *webrtc.DataChannelInit) error {
	if dc := c.dataChannels.Get(label); dc != nil {
		return ErrDataChannelExists
	}

	newDc, err := c.peerConnection.PC().CreateDataChannel(label, initOpts)
	if err != nil {
		return err
	}

	glog.Info("client: data channel created ", label, " ", c.ID())
	c.sfu.setupMessageForwarder(c.ID(), newDc)
	c.dataChannels.Add(newDc)

	return nil
}

func (c *Client) createInternalDataChannel(label string, msgCallback func(msg webrtc.DataChannelMessage)) (*webrtc.DataChannel, error) {
	ordered := true
	newDc, err := c.peerConnection.PC().CreateDataChannel(label, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return nil, err
	}

	newDc.OnMessage(msgCallback)

	return newDc, nil
}

func (c *Client) PublishedTracks() []ITrack {
	return c.publishedTracks.GetTracks()
}

func (c *Client) onInternalMessage(msg webrtc.DataChannelMessage) {
	var internalMessage internalDataMessage

	if err := json.Unmarshal(msg.Data, &internalMessage); err != nil {
		glog.Error("client: error unmarshal internal message ", err)
		return
	}

	switch internalMessage.Type {
	case messageTypeStats:
		internalStats := internalDataStats{}
		if err := json.Unmarshal(msg.Data, &internalStats); err != nil {
			glog.Error("client: error unmarshal messageTypeStats ", err)
			return
		}

		c.onStatsMessage(internalStats.Data)
	case messageTypeVideoSize:
		internalData := internalDataVideoSize{}
		if err := json.Unmarshal(msg.Data, &internalData); err != nil {
			glog.Error("client: error unmarshal messageTypeStats ", err)
			return
		}

		c.bitrateController.onRemoteViewedSizeChanged(internalData.Data)
	}
}

func (c *Client) onStatsMessage(stats remoteClientStats) {
	c.ingressQualityLimitationReason.Store(stats.QualityLimitationReason)

	c.UpdatePublisherBandwidth(uint32(stats.AvailableOutgoingBitrate))
}

// SetReceivingBandwidthLimit will cap the receiving bandwidth and will overide the bandwidth estimation
// if the value is lower than the estimated bandwidth.
// This is useful to test how the SFU will behave when the bandwidth is limited
func (c *Client) SetReceivingBandwidthLimit(bandwidth uint32) {
	c.receivingBandwidth.Store(bandwidth)
}

// SetName update the name of the client, that previously set on create client
// The name then later can use by call client.Name() method
func (c *Client) SetName(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.name = name
}

// TODO: fix the panic nil here when the client is ended
func (c *Client) TrackStats() *ClientTrackStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.peerConnection.PC().ConnectionState() == webrtc.PeerConnectionStateClosed {
		return nil
	}

	clientStats := &ClientTrackStats{
		ID:                       c.id,
		Name:                     c.name,
		ConsumerBandwidth:        c.egressBandwidth.Load(),
		PublisherBandwidth:       c.ingressBandwidth.Load(),
		Sents:                    make([]TrackSentStats, 0),
		Receives:                 make([]TrackReceivedStats, 0),
		CurrentPublishLimitation: c.ingressQualityLimitationReason.Load().(string),
		CurrentConsumerBitrate:   c.bitrateController.totalSentBitrates(),
		VoiceActivityDuration:    uint32(c.stats.VoiceActivity().Milliseconds()),
	}

	for id, stat := range c.stats.Receivers() {
		track, err := c.tracks.Get(id)
		if err != nil {
			continue
		}
		receivedStats := TrackReceivedStats{
			ID:              track.ID(),
			Kind:            track.Kind().String(),
			Codec:           track.MimeType(),
			BytesReceived:   int64(stat.InboundRTPStreamStats.BytesReceived),
			PacketsLost:     stat.InboundRTPStreamStats.PacketsLost,
			PacketsReceived: stat.InboundRTPStreamStats.PacketsReceived,
		}

		clientStats.Receives = append(clientStats.Receives, receivedStats)
	}

	for id, stat := range c.stats.Senders() {
		track, err := c.publishedTracks.Get(id)
		if err != nil {
			continue
		}
		source := "media"

		if track.IsScreen() {
			source = "screen"
		}

		// TODO:
		// - add current bitrate
		sentStats := TrackSentStats{
			ID:           id,
			Kind:         track.Kind().String(),
			PacketsLost:  stat.RemoteInboundRTPStreamStats.PacketsLost,
			PacketSent:   stat.OutboundRTPStreamStats.PacketsSent,
			FractionLost: stat.RemoteInboundRTPStreamStats.FractionLost,
			BytesSent:    stat.OutboundRTPStreamStats.BytesSent,
			Source:       source,
		}

		clientStats.Sents = append(clientStats.Sents, sentStats)
	}

	return clientStats
}

func (c *Client) EnableDebug() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.isDebug = true
}

func (c *Client) IsDebugEnabled() bool {
	return c.isDebug
}

func (c *Client) SFU() *SFU {
	return c.sfu
}

// OnTracksAvailable event is called when the SFU is trying to publish new tracks to the client.
// The client then can subscribe to the tracks by calling `client.SubscribeTracks()` method.
func (c *Client) OnTracksAvailable(callback func([]ITrack)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTracksAvailableCallbacks = append(c.onTracksAvailableCallbacks, callback)
}

func (c *Client) onTracksAvailable(tracks []ITrack) {
	for _, callback := range c.onTracksAvailableCallbacks {
		callback(tracks)
	}
}

// OnVoiceDetected event is called when the SFU is detecting voice activity in the room.
// The callback will receive the voice activity data that can be use for visual indicator of current speaker.
func (c *Client) OnVoiceDetected(callback func(activity voiceactivedetector.VoiceActivity)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onVoiceDetectedCallbacks = append(c.onVoiceDetectedCallbacks, callback)
}

func (c *Client) onVoiceDetected(activity voiceactivedetector.VoiceActivity) {
	for _, callback := range c.onVoiceDetectedCallbacks {
		callback(activity)
	}
}

func (c *Client) IsVADEnabled() bool {
	return c.options.EnableVoiceDetection
}

func (c *Client) enableSendVADToInternalDataChannel() {
	c.OnVoiceDetected(func(activity voiceactivedetector.VoiceActivity) {
		if c.internalDataChannel == nil {
			return
		}

		if c.internalDataChannel.ReadyState() != webrtc.DataChannelStateOpen {
			return
		}

		var dataType string
		if len(activity.AudioLevels) > 0 {
			dataType = messageTypeVADStarted
		} else {
			dataType = messageTypeVADEnded
		}

		dataMessage := internalDataVAD{
			Type: dataType,
			Data: activity,
		}

		data, err := json.Marshal(dataMessage)
		if err != nil {
			glog.Error("client: error marshal vad data ", err)
			return
		}

		if err := c.internalDataChannel.SendText(string(data)); err != nil {
			glog.Error("client: error send vad data ", err)
			return
		}
	})
}

func (c *Client) enableVADStatUpdate() {
	c.OnVoiceDetected(func(activity voiceactivedetector.VoiceActivity) {
		if len(activity.AudioLevels) == 0 {
			c.stats.UpdateVoiceActivity(0)
			return
		}

		for _, data := range activity.AudioLevels {
			duration := data.Timestamp * 1000 / activity.ClockRate
			c.stats.UpdateVoiceActivity(duration)
		}
	})
}

func (c *Client) Tracks() []ITrack {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.tracks.GetTracks()
}

func registerInterceptors(m *webrtc.MediaEngine, interceptorRegistry *interceptor.Registry) error {
	// ConfigureNack will setup everything necessary for handling generating/responding to nack messages.
	generator, err := nack.NewGeneratorInterceptor()
	if err != nil {
		return err
	}

	responder, err := nack.NewResponderInterceptor()
	if err != nil {
		return err
	}

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	interceptorRegistry.Add(responder)
	interceptorRegistry.Add(generator)

	if err := webrtc.ConfigureRTCPReports(interceptorRegistry); err != nil {
		return err
	}

	return webrtc.ConfigureTWCCSender(m, interceptorRegistry)
}
