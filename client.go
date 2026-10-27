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

	"github.com/inlivedev/sfu/pkg/interceptors/playoutdelay"
	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/inlivedev/sfu/pkg/networkmonitor"
	"github.com/inlivedev/sfu/pkg/pacer"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
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

	PacerTypeNoop        = "noop"
	PacerTypeLeakyBucket = "leaky_bucket"

	QualityAudioRed = 11
	QualityAudio    = 10
	QualityHigh     = 9
	QualityHighMid  = 8
	QualityHighLow  = 7
	QualityMid      = 6
	QualityMidMid   = 5
	QualityMidLow   = 4
	QualityLow      = 3
	QualityLowMid   = 2
	QualityLowLow   = 1
	QualityNone     = 0

	messageTypeVideoSize  = "video_size"
	messageTypeStats      = "stats"
	messageTypeVADStarted = "vad_started"
	messageTypeVADEnded   = "vad_ended"
)

type QualityLevel uint32

var (
	ErrNegotiationIsNotRequested = errors.New("client: error negotiation is called before requested")
	ErrRenegotiationCallback     = errors.New("client: error renegotiation callback is not set")
	ErrClientStoped              = errors.New("client: error client already stopped")
)

type ClientOptions struct {
	IceTrickle           bool          `json:"ice_trickle"`
	IdleTimeout          time.Duration `json:"idle_timeout"`
	Type                 string        `json:"type"`
	EnableVoiceDetection bool          `json:"enable_voice_detection"`
	EnablePlayoutDelay   bool          `json:"enable_playout_delay"`
	EnableOpusDTX        bool          `json:"enable_opus_dtx"`
	EnableFlexFEC        bool          `json:"enable_flexfec"`
	EnableOpusInbandFEC  bool          `json:"enable_opus_inband_fec"`
	PacerType            string        `json:"pacer_type"`
	// Configure the minimum playout delay that will be used by the client
	// Recommendation:
	// 0 ms: Certain gaming scenarios (likely without audio) where we will want to play the frame as soon as possible. Also, for remote desktop without audio where rendering a frame asap makes sense
	// 100/150/200 ms: These could be the max target latency for interactive streaming use cases depending on the actual application (gaming, remoting with audio, interactive scenarios)
	// 400 ms: Application that want to ensure a network glitch has very little chance of causing a freeze can start with a minimum delay target that is high enough to deal with network issues. Video streaming is one example.
	MinPlayoutDelay uint16 `json:"min_playout_delay"`
	// Configure the minimum playout delay that will be used by the client
	// Recommendation:
	// 0 ms: Certain gaming scenarios (likely without audio) where we will want to play the frame as soon as possible. Also, for remote desktop without audio where rendering a frame asap makes sense
	// 100/150/200 ms: These could be the max target latency for interactive streaming use cases depending on the actual application (gaming, remoting with audio, interactive scenarios)
	// 400 ms: Application that want to ensure a network glitch has very little chance of causing a freeze can start with a minimum delay target that is high enough to deal with network issues. Video streaming is one example.
	MaxPlayoutDelay     uint16        `json:"max_playout_delay"`
	JitterBufferMinWait time.Duration `json:"jitter_buffer_min_wait"`
	JitterBufferMaxWait time.Duration `json:"jitter_buffer_max_wait"`
	// On unstable network, the packets can be arrived unordered which may affected the nack and packet loss counts, set this to true to allow the SFU to handle reordered packet
	ReorderPackets bool `json:"reorder_packets"`
	Log            logging.LeveledLogger
	settingEngine  webrtc.SettingEngine
	qualityLevels  []QualityLevel
}

type internalDataMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type InternalDataVAD struct {
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
	muTracks              sync.Mutex
	muSimulcastTrack      sync.Mutex
	internalDataChannel   *webrtc.DataChannel
	dataChannels          *DataChannelList
	dataChannelsInitiated bool
	estimator             cc.BandwidthEstimator
	initialReceiverCount  atomic.Uint32
	initialSenderCount    atomic.Uint32
	trackWaitStartTime    time.Time
	isInRenegotiation     *atomic.Bool
	isInRemoteNegotiation *atomic.Bool
	idleTimeoutContext    context.Context
	idleTimeoutCancel     context.CancelFunc
	mu                    sync.Mutex
	peerConnection        *PeerConnection
	// pending received tracks are the remote tracks from other clients that waiting to add when the client is connected
	pendingReceivedTracks []SubscribeTrackRequest
	// pending published tracks are the remote tracks that still state as unknown source, and can't be published until the client state the source media or screen
	// the source can be set through client.SetTracksSourceType()
	pendingPublishedTracks *trackList
	// published tracks are the remote tracks from other clients that are published to this client
	publishedTracks                   *trackList
	pendingRemoteRenegotiation        *atomic.Bool
	negotiations                      atomic.Uint64 // completed negotiations, client tracks re-read their mid and extension IDs when it changes
	receiveRED                        atomic.Bool
	state                             *atomic.Value
	sfu                               *SFU
	muCallback                        sync.Mutex
	onConnectionStateChangedCallbacks []func(webrtc.PeerConnectionState)
	onJoinedCallbacks                 []func()
	onLeftCallbacks                   []func()
	onVoiceSentDetectedCallbacks      []func(voiceactivedetector.VoiceActivity)
	onVoiceReceivedDetectedCallbacks  []func(voiceactivedetector.VoiceActivity)
	onTrackRemovedCallbacks           []func(sourceType string, track *webrtc.TrackLocalStaticRTP)
	onIceCandidate                    func(context.Context, *webrtc.ICECandidate)
	onRenegotiation                   func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)
	onAllowedRemoteRenegotiation      func()
	onTracksAvailableCallbacks        []func([]ITrack)
	onTracksReadyCallbacks            []func([]ITrack)
	onNetworkConditionChangedFunc     func(networkmonitor.NetworkConditionType)
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
	vadInterceptor                 *voiceactivedetector.Interceptor
	vads                           *voiceDetectors
	log                            logging.LeveledLogger

	// negotiationMu makes the checks and updates of isInRenegotiation, isInRemoteNegotiation
	// and negotiationNeeded atomic, so the SFU and the remote client can't both start a negotiation.
	negotiationMu sync.Mutex
	// remoteNegotiationTimer releases a remote negotiation slot that the remote client never used
	remoteNegotiationTimer *time.Timer
}

// voiceDetectors holds the VAD interceptor's detectors by SSRC. The interceptor adds them while binding a remote
// stream, during SetLocalDescription or SetRemoteDescription, and OnTrack looks them up on another goroutine.
type voiceDetectors struct {
	mu   sync.RWMutex
	vads map[uint32]*voiceactivedetector.VoiceDetector
}

func newVoiceDetectors() *voiceDetectors {
	return &voiceDetectors{
		vads: make(map[uint32]*voiceactivedetector.VoiceDetector),
	}
}

func (v *voiceDetectors) set(vad *voiceactivedetector.VoiceDetector) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.vads[vad.SSRC()] = vad
}

func (v *voiceDetectors) get(ssrc uint32) (*voiceactivedetector.VoiceDetector, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	vad, ok := v.vads[ssrc]

	return vad, ok
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		IceTrickle:           true,
		IdleTimeout:          5 * time.Minute,
		Type:                 ClientTypePeer,
		EnableVoiceDetection: true,
		EnablePlayoutDelay:   true,
		EnableOpusDTX:        true,
		EnableOpusInbandFEC:  true,
		PacerType:            PacerTypeNoop,
		MinPlayoutDelay:      150,
		MaxPlayoutDelay:      300,
		JitterBufferMinWait:  20 * time.Millisecond,
		JitterBufferMaxWait:  150 * time.Millisecond,
		ReorderPackets:       false,
		Log:                  logging.NewDefaultLoggerFactory().NewLogger("sfu"),
	}
}

func NewClient(s *SFU, id string, name string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {
	var client *Client
	var vadInterceptor *voiceactivedetector.Interceptor

	localCtx, cancel := context.WithCancel(s.context)
	m := &webrtc.MediaEngine{}

	opts.settingEngine.EnableSCTPZeroChecksum(true)

	if err := RegisterCodecs(m, s.codecs); err != nil {
		panic(err)
	}

	if opts.EnableVoiceDetection {
		voiceactivedetector.RegisterAudioLevelHeaderExtension(m)
	}

	// Create a InterceptorRegistry. This is the user configurable RTP/RTCP Pipeline.
	// This provides NACKs, RTCP Reports and other features. If you use `webrtc.NewPeerConnection`
	// this is enabled by default. If you are manually managing You MUST create a InterceptorRegistry
	// for each PeerConnection.
	i := &interceptor.Registry{}

	if opts.EnableFlexFEC {
		if err := webrtc.ConfigureFlexFEC03(126, m, i); err != nil {
			panic(err)
		}
	}

	estimatorChan := make(chan cc.BandwidthEstimator, 1)

	// leakyBucket is set when the cc interceptor below creates its pacer, so the
	// stream info interceptor can tell the pacer which streams are audio or RTX.
	var leakyBucket atomic.Pointer[pacer.LeakyBucketPacer]

	// Create a Congestion Controller. This analyzes inbound and outbound data and provides
	// suggestions on how much we should be sending.
	//
	// Passing `nil` means we use the default Estimation Algorithm which is Google Congestion Control.
	// You can use the other ones that Pion provides, or write your own!
	congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		var p gcc.Pacer
		switch opts.PacerType {
		case PacerTypeLeakyBucket:
			lb := pacer.NewLeakyBucketPacer(opts.Log, int(s.bitrateConfigs.InitialBandwidth), false)
			leakyBucket.Store(lb)
			p = lb
		case PacerTypeNoop:
			fallthrough
		default:
			p = gcc.NewNoOpPacer()
		}

		// if bw below 100_000, somehow the estimator will struggle to probe the bandwidth and will stuck there. So we set the min to 100_000
		// TODO: we need to use packet loss based bandwidth adjuster when the bandwidth is below 100_000
		bwe, err := gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(int(s.bitrateConfigs.InitialBandwidth)),
			gcc.SendSideBWEPacer(p),
		)
		if err != nil {
			return nil, err
		}

		bwe.OnTargetBitrateChange(func(bitrate int) {
			p.SetTargetBitrate(bitrate)
		})

		return bwe, nil
	})
	if err != nil {
		panic(err)
	}

	congestionController.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
		estimatorChan <- estimator
	})

	// The congestion controller must be registered before the NACK responder from the
	// default interceptors. RTCP readers run in registration order, and cc stores the
	// current target bitrate in the RTCP attributes, which the NACK responder uses to
	// rate limit retransmissions. Registering cc and the TWCC header extension before
	// the NACK responder also makes retransmissions go through the pacer and get their
	// own transport-wide sequence number, so the estimator accounts for them.
	i.Add(congestionController)

	if opts.PacerType == PacerTypeLeakyBucket {
		i.Add(pacer.NewStreamInfoInterceptorFactory(leakyBucket.Load))
	}

	if err = webrtc.ConfigureTWCCHeaderExtensionSender(m, i); err != nil {
		panic(err)
	}

	// Use the default set of Interceptors. The default stats interceptor is shared with the
	// SFU through statsGetter, so packets are only recorded once and pc.GetStats() keeps
	// reporting inbound RTP stats.
	var statsGetter stats.Getter

	captureStatsGetter := func(si *stats.Interceptor) error {
		statsGetter = si

		return nil
	}

	if err = webrtc.RegisterDefaultInterceptorsWithOptions(m, i, webrtc.WithStatsInterceptorOptions(captureStatsGetter)); err != nil {
		panic(err)
	}

	vads := newVoiceDetectors()

	if opts.EnableVoiceDetection {
		opts.Log.Infof("client: voice detection is enabled")
		vadInterceptorFactory := voiceactivedetector.NewInterceptor(localCtx, opts.Log)

		// enable voice detector
		vadInterceptorFactory.OnNew(func(i *voiceactivedetector.Interceptor) {
			vadInterceptor = i
			i.OnNewVAD(func(vad *voiceactivedetector.VoiceDetector) {
				vads.set(vad)
			})
		})

		i.Add(vadInterceptorFactory)
	}

	if opts.EnablePlayoutDelay {
		playoutdelay.RegisterPlayoutDelayHeaderExtension(m)
		playoutDelayInterceptor := playoutdelay.NewInterceptor(opts.Log, opts.MinPlayoutDelay, opts.MaxPlayoutDelay)

		i.Add(playoutDelayInterceptor)
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(opts.settingEngine), webrtc.WithInterceptorRegistry(i)).NewPeerConnection(peerConnectionConfig)
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
		mu:                             sync.Mutex{},
		negotiationNeeded:              &atomic.Bool{},
		peerConnection:                 newPeerConnection(peerConnection),
		state:                          &stateNew,
		tracks:                         newTrackList(opts.Log),
		options:                        opts,
		pendingReceivedTracks:          make([]SubscribeTrackRequest, 0),
		pendingPublishedTracks:         newTrackList(opts.Log),
		pendingRemoteRenegotiation:     &atomic.Bool{},
		publishedTracks:                newTrackList(opts.Log),
		sfu:                            s,
		statsGetter:                    statsGetter,
		quality:                        &quality,
		receivingBandwidth:             &atomic.Uint32{},
		egressBandwidth:                &atomic.Uint32{},
		ingressBandwidth:               &atomic.Uint32{},
		ingressQualityLimitationReason: &atomic.Value{},
		onTracksAvailableCallbacks:     make([]func([]ITrack), 0),
		vadInterceptor:                 vadInterceptor,
		vads:                           vads,
		log:                            opts.Log,
	}

	var trackAddedTimer *time.Timer
	var trackAddedMutex sync.Mutex

	client.onTrack = func(track ITrack) {
		if err := client.pendingPublishedTracks.Add(track); err == ErrTrackExists {
			s.log.Errorf("client: client %s track already added ", track.ID())
			// not an error could be because a simulcast track already added
			return
		}

		trackAddedMutex.Lock()
		defer trackAddedMutex.Unlock()

		// Reset the timer if it's already running
		if trackAddedTimer != nil {
			trackAddedTimer.Stop()
		}

		// Start a new timer for 100ms delay
		var timer *time.Timer
		timer = time.AfterFunc(100*time.Millisecond, func() {
			trackAddedMutex.Lock()
			if trackAddedTimer != timer {
				// a newer track reset the timer, the newer timer will report this track too
				trackAddedMutex.Unlock()
				return
			}
			trackAddedTimer = nil
			trackAddedMutex.Unlock()

			// Don't hold mutex when calling the callback to avoid deadlock
			addedTracks := client.pendingPublishedTracks.GetTracks()
			if len(addedTracks) == 0 {
				return
			}

			s.log.Infof("sfu: client %s publishing %d tracks",
				client.ID(), len(addedTracks))

			if client.onTracksAdded != nil {
				client.onTracksAdded(addedTracks)
			}
		})
		trackAddedTimer = timer
	}

	client.peerConnection.PC().OnSignalingStateChange(func(state webrtc.SignalingState) {
		if state == webrtc.SignalingStateStable {
			client.negotiations.Add(1)
		}

		if state == webrtc.SignalingStateStable && client.pendingRemoteRenegotiation.Load() {
			client.allowRemoteRenegotiation()
		}

		if state == webrtc.SignalingStateStable {
			client.retryRenegotiation()
		}
	})

	peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {

		client.log.Infof("client: connection state changed %s", connectionState.String())

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
							}
						}
					}
				}

				// add relay tracks
				for _, track := range s.relayTracks {
					availableTracks = append(availableTracks, track)
				}

				if len(availableTracks) > 0 {
					client.log.Infof("client: ", client.ID(), " available tracks ", len(availableTracks))
					client.onTracksAvailable(availableTracks)
				}
			}

			client.processPendingTracks()

			// a renegotiation requested before the connection was up is waiting for it
			client.retryRenegotiation()

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
		case webrtc.PeerConnectionState(webrtc.PeerConnectionStateUnknown):
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

	client.bitrateController = newbitrateController(client, opts.qualityLevels)

	go func() {
		estimator := <-estimatorChan
		client.mu.Lock()
		defer client.mu.Unlock()

		client.estimator = estimator
	}()

	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {

		var track ITrack

		remoteTrackID := strings.ReplaceAll(strings.ReplaceAll(remoteTrack.ID(), "{", ""), "}", "")

		defer client.log.Infof("client: new track id %s rid %s ssrc %d kind %s", remoteTrack.ID(), remoteTrack.RID(), remoteTrack.SSRC(), remoteTrack.Kind())

		// make sure the remote track ID is not empty
		if remoteTrackID == "" {
			client.log.Errorf("client: error remote track id is empty")
			return
		}

		onPLI := func() {
			if client.peerConnection == nil || client.peerConnection.PC() == nil || client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
				return
			}

			if err := client.peerConnection.PC().WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{MediaSSRC: uint32(remoteTrack.SSRC())},
			}); err != nil {
				client.log.Errorf("client: error write pli ", err)
			}
		}

		onStatsUpdated := func(stats *stats.Stats) {
			client.stats.SetReceiver(remoteTrack.ID(), remoteTrack.RID(), *stats)
		}

		if remoteTrack.RID() == "" {
			// not simulcast

			minWait := opts.JitterBufferMinWait
			maxWait := opts.JitterBufferMaxWait

			track = newTrack(client.context, client, remoteTrack, receiver.GetParameters().HeaderExtensions, minWait, maxWait, s.pliInterval, onPLI, client.statsGetter, onStatsUpdated)
			track.OnEnded(func() {
				client.stats.removeReceiverStats(remoteTrack.ID() + remoteTrack.RID())
				client.tracks.remove([]string{remoteTrack.ID()})
			})

			if opts.EnableVoiceDetection && remoteTrack.Kind() == webrtc.RTPCodecTypeAudio {
				vad, ok := vads.get(uint32(remoteTrack.SSRC()))
				if ok {
					audioTrack := track.(*AudioTrack)
					audioTrack.SetVAD(vad)
					audioTrack.OnVoiceDetected(func(pkts []voiceactivedetector.VoicePacketData) {
						activity := voiceactivedetector.VoiceActivity{
							TrackID:     track.ID(),
							StreamID:    track.StreamID(),
							SSRC:        uint32(audioTrack.SSRC()),
							ClockRate:   audioTrack.base.codec.ClockRate,
							AudioLevels: pkts,
						}

						client.onVoiceReceiveDetected(activity)
					})
				} else {
					client.log.Errorf("client: error voice detector not found")
				}
			}

			if err := client.tracks.Add(track); err != nil {
				client.log.Errorf("client: error add track ", err)
			}

			client.onTrack(track)
			track.SetAsProcessed()
		} else {
			// simulcast
			var simulcast *SimulcastTrack
			var ok bool

			id := remoteTrack.ID()

			// The layers of a simulcast track arrive in separate OnTrack calls that can run
			// concurrently. Look up or create the track under one lock so only one
			// SimulcastTrack is created and only one call reports it as a new track.
			client.muSimulcastTrack.Lock()

			track, err := client.tracks.Get(id)

			if err != nil {
				// if track not found, add it
				track = newSimulcastTrack(client, remoteTrack, receiver.GetParameters().HeaderExtensions, opts.JitterBufferMinWait, opts.JitterBufferMaxWait, s.pliInterval, onPLI, client.statsGetter, onStatsUpdated)
				if err := client.tracks.Add(track); err != nil {
					client.log.Errorf("client: error add track ", err)
				}

				track.OnEnded(func() {
					simulcastTrack := track.(*SimulcastTrack)

					simulcastTrack.mu.Lock()

					if simulcastTrack.remoteTrackHigh != nil {
						client.stats.removeReceiverStats(simulcastTrack.remoteTrackHigh.track.ID() + simulcastTrack.remoteTrackHigh.track.RID())
					}

					if simulcastTrack.remoteTrackMid != nil {
						client.stats.removeReceiverStats(simulcastTrack.remoteTrackMid.track.ID() + simulcastTrack.remoteTrackMid.track.RID())
					}

					if simulcastTrack.remoteTrackLow != nil {
						client.stats.removeReceiverStats(simulcastTrack.remoteTrackLow.track.ID() + simulcastTrack.remoteTrackLow.track.RID())
					}

					simulcastTrack.mu.Unlock()

					client.tracks.remove([]string{remoteTrack.ID()})
				})

			} else if simulcast, ok = track.(*SimulcastTrack); ok {
				simulcast.AddRemoteTrack(remoteTrack, opts.JitterBufferMinWait, opts.JitterBufferMaxWait, client.statsGetter, onStatsUpdated, onPLI)
			}

			isNewTrack := !track.IsProcessed()
			if isNewTrack {
				track.SetAsProcessed()
			}

			client.muSimulcastTrack.Unlock()

			if isNewTrack {
				client.onTrack(track)
			}

		}
	})

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		// only sending candidate when the local description is set, means expecting the remote peer already has the remote description
		if candidate != nil {
			client.mu.Lock()
			callback := client.onIceCandidate
			if client.canAddCandidate.Load() && callback != nil {
				client.mu.Unlock()
				go callback(client.context, candidate)

				return
			}
			// keep it until the local description is set and the OnIceCandidate callback is set
			client.pendingLocalCandidates = append(client.pendingLocalCandidates, candidate)
			client.mu.Unlock()
		}
	})

	peerConnection.OnNegotiationNeeded(func() {
		client.renegotiate(false)
	})

	return client
}

func (c *Client) initDataChannel() {
	// make sure the exisiting data channels is created on new clients
	c.SFU().createExistingDataChannels(c)

	var internalDataChannel *webrtc.DataChannel
	var err error

	if internalDataChannel, err = c.createInternalDataChannel("internal", c.onInternalMessage); err != nil {
		c.log.Errorf("client: error create internal data channel %s", err.Error())
	}

	c.internalDataChannel = internalDataChannel
}

func (c *Client) ID() string {
	return c.id
}

func (c *Client) Name() string {
	return c.name
}

func (c *Client) Context() context.Context {
	return c.context
}

// OnTrackAdded event is to confirmed the source type of the pending published tracks.
// If the event is not listened, the pending published tracks will be ignored and not published to other clients.
// Once received, respond with `client.SetTracksSourceType()“ to confirm the source type of the pending published tracks
func (c *Client) OnTracksAdded(callback func(addedTracks []ITrack)) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

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
	c.negotiationMu.Lock()
	defer c.negotiationMu.Unlock()

	if c.isInRenegotiation.Load() {
		c.pendingRemoteRenegotiation.Store(true)
		return false
	}

	c.reserveRemoteNegotiationLocked()

	return true
}

// remoteNegotiationTimeout is how long a remote negotiation slot given by IsAllowNegotiation or
// OnAllowedRemoteRenegotiation is kept for the remote client before the SFU can renegotiate again.
const remoteNegotiationTimeout = 5 * time.Second

// reserveRemoteNegotiationLocked blocks the SFU renegotiation until the remote client calls Negotiate().
// If the remote client never calls it, the slot is released after remoteNegotiationTimeout.
// The caller must hold c.negotiationMu.
func (c *Client) reserveRemoteNegotiationLocked() {
	c.isInRemoteNegotiation.Store(true)

	if c.remoteNegotiationTimer != nil {
		c.remoteNegotiationTimer.Stop()
	}

	var timer *time.Timer
	timer = time.AfterFunc(remoteNegotiationTimeout, func() {
		c.negotiationMu.Lock()
		if c.remoteNegotiationTimer != timer {
			c.negotiationMu.Unlock()
			return
		}
		c.remoteNegotiationTimer = nil
		c.isInRemoteNegotiation.Store(false)
		c.negotiationMu.Unlock()

		c.log.Infof("client: %s remote negotiation was allowed but not started, resume the SFU renegotiation", c.ID())
		c.retryRenegotiation()
	})

	c.remoteNegotiationTimer = timer
}

func (c *Client) Negotiate(offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	c.negotiationMu.Lock()
	c.isInRemoteNegotiation.Store(true)
	if c.remoteNegotiationTimer != nil {
		// the remote client is using the slot, don't release it while the negotiation runs
		c.remoteNegotiationTimer.Stop()
		c.remoteNegotiationTimer = nil
	}
	c.negotiationMu.Unlock()

	defer func() {
		c.negotiationMu.Lock()
		c.isInRemoteNegotiation.Store(false)
		c.negotiationMu.Unlock()

		// run the SFU renegotiation that was requested while the remote client was negotiating
		c.retryRenegotiation()
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

	if !c.receiveRED.Load() {
		match, err := regexp.MatchString(`a=rtpmap:63`, offer.SDP)
		if err != nil {
			c.log.Errorf("client: error on check RED support in SDP ", err)
		} else {
			c.receiveRED.Store(match)
		}
	}

	receivers := c.addReceiversForNewRemoteTracks(offer)

	// Set the remote SessionDescription
	err := c.peerConnection.PC().SetRemoteDescription(offer)
	if err != nil {
		c.log.Errorf("client: error set remote description ", err)

		for _, transceiver := range receivers {
			if transceiver.Mid() == "" {
				_ = transceiver.Stop()
			}
		}

		return nil, err
	}

	// Create answer
	answer, err := c.peerConnection.PC().CreateAnswer(nil)
	if err != nil {
		c.log.Errorf("client: error create answer ", err)
		return nil, err
	}

	var gatherComplete <-chan struct{}

	if !c.options.IceTrickle {
		gatherComplete = webrtc.GatheringCompletePromise(c.peerConnection.PC())
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = c.peerConnection.PC().SetLocalDescription(answer)
	if err != nil {
		c.log.Errorf("client: error set local description ", err)
		return nil, err
	}

	if !c.options.IceTrickle {
		<-gatherComplete
	}

	// allow add candidates once the local description is set
	c.canAddCandidate.Store(true)

	// process pending ice, AddICECandidate adds the candidates directly once the remote description is set
	c.mu.Lock()
	pendingRemoteCandidates := c.pendingRemoteCandidates
	c.pendingRemoteCandidates = nil
	c.mu.Unlock()

	for _, iceCandidate := range pendingRemoteCandidates {
		err = c.peerConnection.PC().AddICECandidate(iceCandidate)
		if err != nil {
			c.log.Errorf("client: error add ice candidate ", err)
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

	sdp := c.setOpusSDP(*c.peerConnection.PC().LocalDescription())

	return &sdp, nil
}

// addReceiversForNewRemoteTracks adds a recvonly transceiver for each media section in the offer that
// has a new mid and that the remote client sends on.
// Pion matches a new sendrecv media section (a track added with addTrack) to an unnegotiated local
// transceiver, which can be a sendonly transceiver the SFU just added for a subscribed track.
// The published track would then never be received. Pion picks recvonly transceivers first.
func (c *Client) addReceiversForNewRemoteTracks(offer webrtc.SessionDescription) []*webrtc.RTPTransceiver {
	parsed, err := offer.Unmarshal()
	if err != nil {
		return nil
	}

	mids := make(map[string]bool)
	for _, transceiver := range c.peerConnection.PC().GetTransceivers() {
		if mid := transceiver.Mid(); mid != "" {
			mids[mid] = true
		}
	}

	receivers := make([]*webrtc.RTPTransceiver, 0)

	for _, media := range parsed.MediaDescriptions {
		kind := webrtc.NewRTPCodecType(media.MediaName.Media)
		if kind == 0 {
			continue
		}

		mid, ok := media.Attribute("mid")
		if !ok || mids[mid] {
			continue
		}

		if _, recvOnly := media.Attribute(webrtc.RTPTransceiverDirectionRecvonly.String()); recvOnly {
			continue
		}

		if _, inactive := media.Attribute(webrtc.RTPTransceiverDirectionInactive.String()); inactive {
			continue
		}

		transceiver, err := c.peerConnection.PC().AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		if err != nil {
			c.log.Errorf("client: error add receiver for remote track ", err)
			continue
		}

		receivers = append(receivers, transceiver)
	}

	return receivers
}

func (c *Client) setOpusSDP(sdp webrtc.SessionDescription) webrtc.SessionDescription {
	if c.options.EnableOpusDTX {
		var regex, err = regexp.Compile(`a=rtpmap:(\d+) opus\/(\d+)\/(\d+)`)
		if err != nil {
			c.log.Errorf("client: error on compile regex ", err)
			return sdp
		}
		var opusLine = regex.FindString(sdp.SDP)

		if opusLine == "" {
			c.log.Errorf("client: error opus line not found")
			return sdp
		}

		regex, err = regexp.Compile(`(\d+)`)
		if err != nil {
			c.log.Errorf("client: error on compile regex ", err)
			return sdp
		}

		var opusNo = regex.FindString(opusLine)
		if opusNo == "" {
			c.log.Errorf("client: error opus no not found")
			return sdp
		}

		fmtpRegex, err := regexp.Compile(`a=fmtp:` + opusNo + ` .+`)
		if err != nil {
			c.log.Errorf("client: error on compile regex ", err)
			return sdp
		}

		var fmtpLine = fmtpRegex.FindString(sdp.SDP)
		if fmtpLine == "" {
			c.log.Errorf("client: error fmtp line not found")
			return sdp
		}

		var newFmtpLine = ""

		if c.options.EnableOpusDTX && !strings.Contains(fmtpLine, "usedtx=1") {
			newFmtpLine += ";usedtx=1"
		}

		if c.options.EnableOpusInbandFEC && !strings.Contains(fmtpLine, "useinbandfec=1") {
			newFmtpLine += ";useinbandfec=1"
		}

		if newFmtpLine != "" {
			sdp.SDP = strings.Replace(sdp.SDP, fmtpLine, fmtpLine+newFmtpLine, -1)
		}
	}

	if !c.dataChannelsInitiated {
		c.initDataChannel()
		c.dataChannelsInitiated = true
	}

	return sdp
}

// OnRenegotiation event is called when the SFU is trying to renegotiate with the client.
// The callback will receive the SDP offer from the SFU that must be use to create the SDP answer from the client.
// The SDP answer then can be passed back to the SFU using `client.CompleteNegotiation()` method.
func (c *Client) OnRenegotiation(callback func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)) {
	c.muCallback.Lock()
	c.onRenegotiation = callback
	c.muCallback.Unlock()

	// run a renegotiation that was requested before the callback was set
	c.retryRenegotiation()
}

func (c *Client) getOnRenegotiation() func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	return c.onRenegotiation
}

func (c *Client) renegotiate(offerFlexFec bool) {
	c.log.Debug("client: renegotiate")
	// set before taking negotiationMu, a running renegotiation loop checks it under the lock before it stops
	c.negotiationNeeded.Store(true)

	if c.getOnRenegotiation() == nil {
		c.log.Errorf("client: onRenegotiation is not set, can't do renegotiation")

		return
	}

	c.negotiationMu.Lock()
	defer c.negotiationMu.Unlock()

	if c.isInRemoteNegotiation.Load() {
		// retried when the remote negotiation is done
		c.log.Infof("sfu: renegotiation is delayed because the remote client %s is doing negotiation ", c.ID())

		return
	}

	// no need to run another negotiation if it's already in progress, it will rerun because we mark the negotiationneeded to true
	if c.isInRenegotiation.Load() {
		c.log.Infof("sfu: renegotiation is delayed because the client %s is doing negotiation ", c.ID())
		return
	}

	// mark negotiation is in progress to make sure no concurrent negotiation
	c.isInRenegotiation.Store(true)

	go c.renegotiationLoop(offerFlexFec)
}

// retryRenegotiation runs a renegotiation that was requested but couldn't run yet,
// because the remote client was negotiating, the signaling state wasn't stable or the client wasn't connected.
func (c *Client) retryRenegotiation() {
	if c.negotiationNeeded.Load() && c.state.Load() != ClientStateEnded {
		c.renegotiate(false)
	}
}

// canRenegotiateLocked reports whether the SFU can send an offer now. The caller must hold c.negotiationMu.
func (c *Client) canRenegotiateLocked() bool {
	return c.state.Load() != ClientStateEnded &&
		c.getOnRenegotiation() != nil &&
		!c.isInRemoteNegotiation.Load() &&
		c.peerConnection.PC().SignalingState() == webrtc.SignalingStateStable &&
		c.peerConnection.PC().ConnectionState() == webrtc.PeerConnectionStateConnected
}

func (c *Client) renegotiationLoop(offerFlexFec bool) {
	defer func() {
		c.negotiationMu.Lock()
		c.isInRenegotiation.Store(false)
		c.negotiationMu.Unlock()

		if c.pendingRemoteRenegotiation.Load() {
			c.allowRemoteRenegotiation()
		}
	}()

	for {
		// wait a bit to batch the renegotiation requests
		select {
		case <-c.context.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}

		c.negotiationMu.Lock()
		if !c.negotiationNeeded.Load() || !c.canRenegotiateLocked() {
			// keep negotiationNeeded, retryRenegotiation runs it once the client is ready
			c.isInRenegotiation.Store(false)
			c.negotiationMu.Unlock()

			return
		}

		// mark negotiation is not needed, a request after this point will run another iteration
		c.negotiationNeeded.Store(false)
		c.negotiationMu.Unlock()

		onRenegotiation := c.getOnRenegotiation()
		if onRenegotiation == nil {
			return
		}

		offer, err := c.peerConnection.PC().CreateOffer(nil)
		if err != nil {
			c.log.Errorf("sfu: error create offer on renegotiation ", err)
			return
		}

		if offerFlexFec {
			// munge the offer to include FlexFEC
			// get the payload code of video track

		}

		// Sets the LocalDescription, and starts our UDP listeners
		err = c.peerConnection.PC().SetLocalDescription(offer)
		if err != nil {
			c.log.Errorf("sfu: error set local description on renegotiation ", err)
			_ = c.stop()

			return
		}

		// this will be blocking until the renegotiation is done
		sdp := c.setOpusSDP(*c.peerConnection.PC().LocalDescription())
		answer, err := onRenegotiation(c.context, sdp)
		if err != nil {
			//TODO: when this happen, we need to close the client and ask the remote client to reconnect
			c.log.Errorf("sfu: error on renegotiation ", err)
			_ = c.stop()

			return
		}

		if answer.Type != webrtc.SDPTypeAnswer {
			c.log.Errorf("sfu: error on renegotiation, the answer is not an answer type")
			_ = c.stop()

			return
		}

		err = c.peerConnection.PC().SetRemoteDescription(answer)
		if err != nil {
			_ = c.stop()

			return
		}
	}
}

// OnAllowedRemoteRenegotiation event is called when the SFU is done with the renegotiation
// and ready to receive the renegotiation from the client.
// Use this event to trigger the client to do renegotiation if needed.
func (c *Client) OnAllowedRemoteRenegotiation(callback func()) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	c.onAllowedRemoteRenegotiation = callback
}

// inform to remote client that it's allowed to do renegotiation through event
// The remote client gets the negotiation slot, but if it doesn't call Negotiate() the slot is released
// after remoteNegotiationTimeout so the SFU renegotiation isn't blocked.
func (c *Client) allowRemoteRenegotiation() {
	c.muCallback.Lock()
	onAllowedRemoteRenegotiation := c.onAllowedRemoteRenegotiation
	c.muCallback.Unlock()

	if onAllowedRemoteRenegotiation == nil {
		return
	}

	c.negotiationMu.Lock()
	if c.isInRenegotiation.Load() {
		// the running renegotiation loop allows it when it's done
		c.negotiationMu.Unlock()
		return
	}

	if !c.pendingRemoteRenegotiation.CompareAndSwap(true, false) {
		c.negotiationMu.Unlock()
		return
	}

	c.reserveRemoteNegotiationLocked()
	c.negotiationMu.Unlock()

	go onAllowedRemoteRenegotiation()
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
		if t.Kind() == webrtc.RTPCodecTypeAudio {
			singleTrack := t.(*AudioTrack)
			outputTrack = singleTrack.subscribe(c)
		} else {
			singleTrack := t.(*Track)
			outputTrack = singleTrack.subscribe(c)
		}

	}

	localTrack := outputTrack.LocalTrack()

	senderTcv, err := c.peerConnection.PC().AddTransceiverFromTrack(localTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		c.log.Errorf("client: error on adding track ", err)
		return nil
	}

	outputTrack.setTransceiver(senderTcv)

	// TODO: change to non goroutine

	outputTrack.OnEnded(func() {
		if c == nil {
			return
		}

		defer func() {
			c.muTracks.Lock()
			delete(c.clientTracks, outputTrack.ID())
			c.publishedTracks.remove([]string{outputTrack.ID()})
			c.muTracks.Unlock()
		}()

		sender := senderTcv.Sender()

		if sender == nil {
			return
		}

		c.peerConnection.PC().RemoveTrack(sender)
	})

	// enable RTCP report and stats
	c.enableReportAndStats(senderTcv.Sender(), outputTrack)

	c.muTracks.Lock()
	c.clientTracks[outputTrack.ID()] = outputTrack
	c.muTracks.Unlock()

	return outputTrack
}

func (c *Client) ClientTracks() map[string]iClientTrack {
	c.muTracks.Lock()
	defer c.muTracks.Unlock()

	clientTracks := make(map[string]iClientTrack, 0)
	for k, v := range c.clientTracks {
		clientTracks[k] = v
	}

	return clientTracks
}

func readRTCP(r *webrtc.RTPSender, b []byte) ([]rtcp.Packet, interceptor.Attributes, error) {
	n, attributes, err := r.Read(b)
	if err != nil {
		return nil, nil, err
	}

	pkts, err := attributes.GetRTCPPackets(b[:n])
	if err != nil {
		b = b[:0]
		return nil, nil, err
	}

	b = b[:0]

	return pkts, attributes, nil
}

// TODO: need to improve and reduce goroutine usage
func (c *Client) enableReportAndStats(rtpSender *webrtc.RTPSender, track iClientTrack) {
	ssrc := rtpSender.GetParameters().Encodings[0].SSRC
	go func() {
		localCtx, cancel := context.WithCancel(track.Context())
		defer cancel()

		clientCtx, cancelClientCtx := context.WithCancel(c.context)
		defer cancelClientCtx()

		buff := make([]byte, 1500)

		for {
			select {
			case <-clientCtx.Done():
				return
			case <-localCtx.Done():
				return
			default:
				rtcpPackets, _, err := readRTCP(rtpSender, buff)
				if err != nil && (err == io.EOF || err == io.ErrClosedPipe) {
					return
				}

				for _, p := range rtcpPackets {
					switch p.(type) {
					case *rtcp.PictureLossIndication:
						c.log.Tracef("client: received PLI from %d", ssrc)
						track.RequestPLI()
					case *rtcp.FullIntraRequest:
						c.log.Tracef("client: received PLI from %d", ssrc)
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

		clientCtx, cancelClientCtx := context.WithCancel(c.context)
		defer cancelClientCtx()

		for {
			select {
			case <-clientCtx.Done():
				return
			case <-localCtx.Done():
				return
			case <-tick.C:
				c.updateSenderStats(rtpSender, ssrc)
			}
		}
	}()
}

func (c *Client) processPendingTracks() {
	// SubscribeTracks appends to the pending list under c.mu while the client
	// is still connecting, so take it under the same lock.
	c.mu.Lock()
	pending := c.pendingReceivedTracks
	c.pendingReceivedTracks = make([]SubscribeTrackRequest, 0)
	c.mu.Unlock()

	if len(pending) > 0 {
		err := c.SubscribeTracks(pending)
		if err != nil {
			c.log.Errorf("client: error subscribe tracks %s ", err.Error())
		}
	}
}

// make sure to call this when client's done to clean everything
func (c *Client) afterClosed() {
	c.mu.Lock()
	state := c.state.Load()
	if state == ClientStateEnded {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	c.state.Store(ClientStateEnded)

	if c.internalDataChannel != nil {
		c.internalDataChannel.Close()
	}

	c.dataChannels.Clear()

	c.onLeft()

	c.sfu.onAfterClientStopped(c)

	c.cancel()
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

// End the client connection and clean up the resources.
func (c *Client) End() error {
	err := c.stop()
	if err != nil {
		c.log.Errorf("client: error stop client %s", err.Error())
	}

	return err
}

func (c *Client) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if c.peerConnection == nil || c.peerConnection.PC() == nil {
		return errors.New("client: peer connection is not initialized, make sure to call this after first negotiation done")
	}

	c.mu.Lock()
	if c.peerConnection.PC().RemoteDescription() == nil {
		c.pendingRemoteCandidates = append(c.pendingRemoteCandidates, candidate)
		c.mu.Unlock()
	} else {
		c.mu.Unlock()
		if err := c.peerConnection.PC().AddICECandidate(candidate); err != nil {
			c.log.Errorf("client: error add ice candidate ", err)
			return err
		}
	}

	return nil
}

// OnTracksAvailable event is called when the SFU has ice candidate that need to pass to the client.
// This event will triggered during negotiation process to exchanges ice candidates between SFU and client.
// The client can also pass the ice candidate to the SFU using `client.AddICECandidate()` method.
func (c *Client) OnIceCandidate(callback func(context.Context, *webrtc.ICECandidate)) {
	c.mu.Lock()
	c.onIceCandidate = callback
	c.mu.Unlock()

	// send the candidates gathered before the callback was set
	if c.canAddCandidate.Load() {
		go c.sendPendingLocalCandidates()
	}
}

func (c *Client) sendPendingLocalCandidates() {
	c.mu.Lock()
	callback := c.onIceCandidate
	if callback == nil {
		c.mu.Unlock()
		c.log.Infof("client: on ice candidate callback is not set")
		return
	}

	candidates := c.pendingLocalCandidates
	c.pendingLocalCandidates = nil
	c.mu.Unlock()

	for _, candidate := range candidates {
		callback(c.context, candidate)
	}
}

// OnConnectionStateChanged event is called when the SFU connection state is changed.
// The callback will receive the connection state as the new state.
func (c *Client) OnConnectionStateChanged(callback func(webrtc.PeerConnectionState)) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	c.onConnectionStateChangedCallbacks = append(c.onConnectionStateChangedCallbacks, callback)
}

func (c *Client) onConnectionStateChanged(state webrtc.PeerConnectionState) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	for _, callback := range c.onConnectionStateChangedCallbacks {
		go callback(webrtc.PeerConnectionState(state))
	}
}

func (c *Client) onJoined() {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	for _, callback := range c.onJoinedCallbacks {
		callback()
	}
}

// OnJoined event is called when the client is joined to the room.
// This doesn't mean that the client's tracks are already published to the room.
// This event can be use to track number of clients in the room.
func (c *Client) OnJoined(callback func()) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	c.onJoinedCallbacks = append(c.onJoinedCallbacks, callback)
}

// OnLeft event is called when the client is left from the room.
// This event can be use to track number of clients in the room.
func (c *Client) OnLeft(callback func()) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	c.onLeftCallbacks = append(c.onLeftCallbacks, callback)
}

func (c *Client) onLeft() {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

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

	go func() {
		c.idleTimeoutContext, c.idleTimeoutCancel = context.WithTimeout(c.context, timeout)
		<-c.idleTimeoutContext.Done()
		if c == nil || c.idleTimeoutContext == nil || c.idleTimeoutCancel == nil {
			return
		}

		defer c.idleTimeoutCancel()

		err := c.idleTimeoutContext.Err()
		if err != nil && err == context.DeadlineExceeded {
			c.log.Infof("client: idle timeout reached ", c.ID)

			err := c.stop()
			if err != nil {
				c.log.Errorf("client: error stop client ", err)
			}
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

func (c *Client) updateSenderStats(sender *webrtc.RTPSender, ssrc webrtc.SSRC) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.statsGetter == nil ||
		c.stats == nil {

		return
	}

	stats := c.statsGetter.Get(uint32(ssrc))
	if stats != nil && sender != nil && sender.Track() != nil {
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
		c.log.Debugf("client: %s set source tracks %d", c.ID(), len(availableTracks))
		c.sfu.onTracksAvailable(c.ID(), availableTracks)
		c.onTracksReady(availableTracks)
	}
}

// SubscribeTracks subscribe tracks from other clients that are published to this client
// The client must listen for `client.OnTracksAvailable` to know if a new track is available to subscribe.
// Calling subscribe tracks will trigger the SFU renegotiation with the client.
func (c *Client) SubscribeTracks(req []SubscribeTrackRequest) error {
	if c.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		c.mu.Lock()
		c.pendingReceivedTracks = append(c.pendingReceivedTracks, req...)
		c.mu.Unlock()

		return nil
	}

	clientTracks := make([]iClientTrack, 0)

	for _, r := range req {
		trackFound := false

		// skip track if it's own track
		if c.ID() == r.ClientID {
			continue
		}

		client, err := c.sfu.clients.GetClient(r.ClientID)
		if err != nil {
			return err
		}

		for _, track := range client.tracks.GetTracks() {
			if track.ID() == r.TrackID {
				if clientTrack := c.setClientTrack(track); clientTrack != nil {
					clientTracks = append(clientTracks, clientTrack)
				}

				c.log.Debugf("client: subscribe track %s from %s to %s", r.TrackID, r.ClientID, c.ID())

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

		if !trackFound {
			return fmt.Errorf("client: track %s not found", r.TrackID)
		}
	}

	if len(clientTracks) > 0 {
		// claim bitrates
		if err := c.bitrateController.addClaims(clientTracks); err != nil {
			c.log.Errorf("client: failed to add claims ", err)
		}

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

	c.log.Infof("client: %s switch quality to %s", c.ID, quality)
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

	if c.estimator == nil {
		return c.sfu.bitrateConfigs.InitialBandwidth
	}

	// overshot the bandwidth by 40%
	return uint32(c.estimator.GetTargetBitrate() * 1400 / 1000)
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

	c.log.Infof("client: data channel created ", label, " ", c.ID())
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
		c.log.Errorf("client: error unmarshal internal message ", err)
		return
	}

	switch internalMessage.Type {
	case messageTypeStats:
		internalStats := internalDataStats{}
		if err := json.Unmarshal(msg.Data, &internalStats); err != nil {
			c.log.Errorf("client: error unmarshal messageTypeStats ", err)
			return
		}

		c.onStatsMessage(internalStats.Data)
	case messageTypeVideoSize:
		internalData := internalDataVideoSize{}
		if err := json.Unmarshal(msg.Data, &internalData); err != nil {
			c.log.Errorf("client: error unmarshal messageTypeStats ", err)
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
func (c *Client) Stats() ClientTrackStats {
	if c.peerConnection.PC().ConnectionState() == webrtc.PeerConnectionStateClosed {
		return ClientTrackStats{}
	}

	clientStats := ClientTrackStats{
		ID:                       c.id,
		Name:                     c.name,
		ConsumerBandwidth:        c.GetEstimatedBandwidth(),
		PublisherBandwidth:       c.ingressBandwidth.Load(),
		Sents:                    make([]TrackSentStats, 0),
		Receives:                 make([]TrackReceivedStats, 0),
		CurrentPublishLimitation: c.ingressQualityLimitationReason.Load().(string),
		CurrentConsumerBitrate:   c.bitrateController.totalSentBitrates(),
		VoiceActivityDurationMS:  uint32(c.stats.VoiceActivity().Milliseconds()),
	}

	for _, track := range c.Tracks() {
		if track.IsSimulcast() {
			simulcastClientTrack := track.(*SimulcastTrack)
			if simulcastClientTrack.remoteTrackHigh != nil {
				stats, err := c.stats.GetReceiver(simulcastClientTrack.remoteTrackHigh.Track().ID(), simulcastClientTrack.remoteTrackHigh.Track().RID())
				if err == nil {
					receivedStats, err := generateClientReceiverStats(c, simulcastClientTrack.remoteTrackHigh.Track(), stats)
					if err == nil {
						clientStats.Receives = append(clientStats.Receives, receivedStats)
					}
				}

			}

			if simulcastClientTrack.remoteTrackMid != nil {
				stats, err := c.stats.GetReceiver(simulcastClientTrack.remoteTrackMid.Track().ID(), simulcastClientTrack.remoteTrackMid.Track().RID())
				if err == nil {
					receivedStats, err := generateClientReceiverStats(c, simulcastClientTrack.remoteTrackMid.Track(), stats)
					if err == nil {
						clientStats.Receives = append(clientStats.Receives, receivedStats)
					}
				}

			}

			if simulcastClientTrack.remoteTrackLow != nil {
				stats, err := c.stats.GetReceiver(simulcastClientTrack.remoteTrackLow.Track().ID(), simulcastClientTrack.remoteTrackLow.Track().RID())
				if err == nil {
					receivedStats, err := generateClientReceiverStats(c, simulcastClientTrack.remoteTrackLow.Track(), stats)
					if err == nil {
						clientStats.Receives = append(clientStats.Receives, receivedStats)
					}
				}

			}

		} else {
			var receivedStats TrackReceivedStats

			if track.Kind() == webrtc.RTPCodecTypeAudio {
				t := track.(*AudioTrack)
				stat, err := c.stats.GetReceiver(t.RemoteTrack().track.ID(), t.RemoteTrack().track.RID())
				if err != nil {
					continue
				}

				receivedStats, err = generateClientReceiverStats(c, t.RemoteTrack().Track(), stat)
				if err != nil {
					continue
				}
			} else {
				t := track.(*Track)
				stat, err := c.stats.GetReceiver(t.RemoteTrack().track.ID(), t.RemoteTrack().track.RID())
				if err != nil {
					continue
				}

				receivedStats, err = generateClientReceiverStats(c, t.RemoteTrack().Track(), stat)
				if err != nil {
					continue
				}
			}

			clientStats.Receives = append(clientStats.Receives, receivedStats)
		}

	}

	for id, stat := range c.stats.Senders() {
		c.muTracks.Lock()
		track, ok := c.clientTracks[id]
		c.muTracks.Unlock()
		if !ok {
			continue
		}
		source := "media"

		if track.IsScreen() {
			source = "screen"
		}

		sentStats := TrackSentStats{
			ID:             id,
			StreamID:       track.StreamID(),
			Kind:           track.Kind(),
			Codec:          track.MimeType(),
			PacketsLost:    stat.RemoteInboundRTPStreamStats.PacketsLost,
			PacketSent:     stat.OutboundRTPStreamStats.PacketsSent,
			FractionLost:   stat.RemoteInboundRTPStreamStats.FractionLost,
			BytesSent:      sentWireBytes(stat),
			CurrentBitrate: track.SendBitrate(),
			Source:         source,
			Quality:        track.Quality(),
			MaxQuality:     track.MaxQuality(),
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
// The callback will receive the list of tracks from other clients that are available to subscribe.
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

// OnTracksReady event is called when the client's tracks are use from the client
// This can be use to hook a processing like transcription/video processing to client published tracks
func (c *Client) OnTracksReady(callback func([]ITrack)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTracksReadyCallbacks = append(c.onTracksReadyCallbacks, callback)
}

func (c *Client) onTracksReady(tracks []ITrack) {
	for _, callback := range c.onTracksReadyCallbacks {
		callback(tracks)
	}
}

// OnVoiceDetected event is called when the SFU is detecting voice activity in the room.
// The callback will receive the voice activity data that can be use for visual indicator of current speaker.
func (c *Client) OnVoiceSentDetected(callback func(activity voiceactivedetector.VoiceActivity)) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	c.onVoiceSentDetectedCallbacks = append(c.onVoiceSentDetectedCallbacks, callback)
}

func (c *Client) onVoiceSentDetected(activity voiceactivedetector.VoiceActivity) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()
	for _, callback := range c.onVoiceSentDetectedCallbacks {
		callback(activity)
	}
}

func (c *Client) IsVADEnabled() bool {
	return c.options.EnableVoiceDetection
}

func (c *Client) enableSendVADToInternalDataChannel() {
	c.OnVoiceSentDetected(func(activity voiceactivedetector.VoiceActivity) {
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

		if len(activity.AudioLevels) == 0 {
			c.sendVad(dataType, activity)
			return
		}

		maxLength := 2048

		for len(activity.AudioLevels) > 0 {
			dataLength := min(len(activity.AudioLevels), maxLength)
			audioLevels := activity.AudioLevels[:dataLength]
			activity.AudioLevels = activity.AudioLevels[dataLength:]
			c.sendVad(dataType, voiceactivedetector.VoiceActivity{
				TrackID:     activity.TrackID,
				StreamID:    activity.StreamID,
				SSRC:        activity.SSRC,
				ClockRate:   activity.ClockRate,
				AudioLevels: audioLevels,
			})
		}
	})
}

func (c *Client) sendVad(dataType string, activity voiceactivedetector.VoiceActivity) {
	dataMessage := InternalDataVAD{
		Type: dataType,
		Data: activity,
	}

	data, err := json.Marshal(dataMessage)
	if err != nil {
		c.log.Errorf("client: error marshal vad data ", err)
		return
	}

	if err := c.internalDataChannel.SendText(string(data)); err != nil {
		c.log.Errorf("client: error send vad data ", err)
		c.log.Errorf("client: voice activity count ", len(activity.AudioLevels))
		return
	}
}

func (c *Client) OnVoiceReceivedDetected(callback func(activity voiceactivedetector.VoiceActivity)) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	c.onVoiceReceivedDetectedCallbacks = append(c.onVoiceReceivedDetectedCallbacks, callback)
}

func (c *Client) enableVADStatUpdate() {
	c.OnVoiceReceivedDetected(func(activity voiceactivedetector.VoiceActivity) {
		if len(activity.AudioLevels) == 0 {
			c.stats.UpdateVoiceActivity(0, 0)
			return
		}

		for _, data := range activity.AudioLevels {
			c.stats.UpdateVoiceActivity(data.Timestamp, activity.ClockRate)
		}
	})
}

func (c *Client) onVoiceReceiveDetected(activity voiceactivedetector.VoiceActivity) {
	c.muCallback.Lock()
	defer c.muCallback.Unlock()

	for _, callback := range c.onVoiceReceivedDetectedCallbacks {
		callback(activity)
	}
}

func (c *Client) Tracks() []ITrack {
	return c.tracks.GetTracks()
}

func generateClientReceiverStats(c *Client, track IRemoteTrack, stat stats.Stats) (TrackReceivedStats, error) {
	bitrate, _ := c.stats.GetReceiverBitrate(track.ID(), track.RID())

	receivedStats := TrackReceivedStats{
		ID:              track.ID(),
		RID:             track.RID(),
		StreamID:        track.StreamID(),
		Kind:            track.Kind(),
		Codec:           track.Codec().MimeType,
		BytesReceived:   int64(receivedWireBytes(stat)),
		CurrentBitrate:  bitrate,
		PacketsLost:     stat.InboundRTPStreamStats.PacketsLost,
		PacketsReceived: stat.InboundRTPStreamStats.PacketsReceived,
	}

	return receivedStats, nil
}

func (c *Client) OnNetworkConditionChanged(callback func(networkmonitor.NetworkConditionType)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onNetworkConditionChangedFunc = callback
}

func (c *Client) onNetworkConditionChanged(condition networkmonitor.NetworkConditionType) {
	if c.onNetworkConditionChangedFunc != nil {
		c.onNetworkConditionChangedFunc(condition)
	}
}
