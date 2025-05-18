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
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/nack"
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
	internalDataChannel   *webrtc.DataChannel
	dataChannels          *DataChannelList
	dataChannelsInitiated bool
	estimator             cc.BandwidthEstimator
	initialReceiverCount  atomic.Uint32
	initialSenderCount    atomic.Uint32
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
	receiveRED                        bool
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
	vads                           map[uint32]*voiceactivedetector.VoiceDetector
	log                            logging.LeveledLogger
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

	var vads = make(map[uint32]*voiceactivedetector.VoiceDetector)

	if opts.EnableVoiceDetection {
		opts.Log.Infof("client: voice detection is enabled")
		vadInterceptorFactory := voiceactivedetector.NewInterceptor(localCtx, opts.Log)

		// enable voice detector
		vadInterceptorFactory.OnNew(func(i *voiceactivedetector.Interceptor) {
			vadInterceptor = i
			i.OnNewVAD(func(vad *voiceactivedetector.VoiceDetector) {
				vads[vad.SSRC()] = vad
			})
		})

		i.Add(vadInterceptorFactory)
	}

	if opts.EnableFlexFEC {
		flexfecInterceptor, err := flexfec.NewFecInterceptor()
		if err != nil {
			panic(err)
		}

		i.Add(flexfecInterceptor)
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
			// gcc.SendSideBWEPacer(pacer.NewLeakyBucketPacer(opts.Log, int(s.bitrateConfigs.InitialBandwidth), true)),
			gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
			// gcc.SendSideBWEPacer(gcc.NewLeakyBucketPacer(int(s.bitrateConfigs.InitialBandwidth))),
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
		playoutDelayInterceptor := playoutdelay.NewInterceptor(opts.Log, opts.MinPlayoutDelay, opts.MaxPlayoutDelay)

		i.Add(playoutDelayInterceptor)
	}

	// Use the default set of Interceptors
	if err := registerInterceptors(m, i); err != nil {
		panic(err)
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

	client.onTrack = func(track ITrack) {
		if err := client.pendingPublishedTracks.Add(track); err == ErrTrackExists {
			s.log.Errorf("client: client %s track already added ", track.ID())
			// not an error could be because a simulcast track already added
			return
		}

		addedTracks := client.pendingPublishedTracks.GetTracks()

		if client.onTracksAdded != nil {
			client.onTracksAdded(addedTracks)
		}
	}

	client.peerConnection.PC().OnSignalingStateChange(func(state webrtc.SignalingState) {
		if state == webrtc.SignalingStateStable && client.pendingRemoteRenegotiation.Load() {
			client.pendingRemoteRenegotiation.Store(false)
			client.allowRemoteRenegotiation()
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
							} else {
								c.log.Errorf("client: track already exists")
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

			if len(client.pendingReceivedTracks) > 0 {
				client.processPendingTracks()
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

			track = newTrack(client.context, client, remoteTrack, minWait, maxWait, s.pliInterval, onPLI, client.statsGetter, onStatsUpdated)
			track.OnEnded(func() {
				client.stats.removeReceiverStats(remoteTrack.ID() + remoteTrack.RID())
				client.tracks.remove([]string{remoteTrack.ID()})
			})

			if opts.EnableVoiceDetection && remoteTrack.Kind() == webrtc.RTPCodecTypeAudio {
				vad, ok := vads[uint32(remoteTrack.SSRC())]
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

			track, err = client.tracks.Get(id) // not found because the track is not added yet due to race condition

			if err != nil {
				// if track not found, add it
				track = newSimulcastTrack(client, remoteTrack, opts.JitterBufferMinWait, opts.JitterBufferMaxWait, s.pliInterval, onPLI, client.statsGetter, onStatsUpdated)
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

			if !track.IsProcessed() {
				client.onTrack(track)
				track.SetAsProcessed()
			}

		}
	})

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		// only sending candidate when the local description is set, means expecting the remote peer already has the remote description
		if candidate != nil {
			if client.canAddCandidate.Load() {
				go client.onIceCandidateCallback(candidate)

				return
			}
			client.mu.Lock()
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
	if c.isInRenegotiation.Load() {
		c.pendingRemoteRenegotiation.Store(true)
		return false
	}

	c.isInRemoteNegotiation.Store(true)

	return true
}

func (c *Client) Negotiate(offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	c.isInRemoteNegotiation.Store(true)

	defer func() {
		c.isInRemoteNegotiation.Store(false)
		if c.negotiationNeeded.Load() {
			c.renegotiate(false)
		}
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
			c.log.Errorf("client: error on check RED support in SDP ", err)
		} else {
			c.receiveRED = match
		}
	}

	// Set the remote SessionDescription
	err := c.peerConnection.PC().SetRemoteDescription(offer)
	if err != nil {
		c.log.Errorf("client: error set remote description ", err)

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

	// process pending ice
	for _, iceCandidate := range c.pendingRemoteCandidates {
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

	c.pendingRemoteCandidates = nil

	sdp := c.setOpusSDP(*c.peerConnection.PC().LocalDescription())

	return &sdp, nil
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
	defer c.muCallback.Unlock()

	c.onRenegotiation = callback
}

func (c *Client) renegotiate(offerFlexFec bool) {
	c.log.Debug("client: renegotiate")
	c.negotiationNeeded.Store(true)

	if c.onRenegotiation == nil {
		c.log.Errorf("client: onRenegotiation is not set, can't do renegotiation")

		return
	}

	if c.isInRemoteNegotiation.Load() {
		c.log.Infof("sfu: renegotiation is delayed because the remote client %s is doing negotiation ", c.ID)

		return
	}

	// no need to run another negotiation if it's already in progress, it will rerun because we mark the negotiationneeded to true
	if c.isInRenegotiation.Load() {
		c.log.Infof("sfu: renegotiation is delayed because the client %s is doing negotiation ", c.ID)
		return
	}

	// mark negotiation is in progress to make sure no concurrent negotiation
	c.isInRenegotiation.Store(true)

	go func() {
		defer func() {
			c.isInRenegotiation.Store(false)
			if c.pendingRemoteRenegotiation.Load() {
				c.allowRemoteRenegotiation()
			}
		}()

		for c.negotiationNeeded.Load() {
			timout, cancel := context.WithTimeout(c.context, 100*time.Millisecond)
			defer cancel()

			<-timout.Done()

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
				answer, err := c.onRenegotiation(c.context, sdp)
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
	}()

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
func (c *Client) allowRemoteRenegotiation() {
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
	if len(c.pendingReceivedTracks) > 0 {
		err := c.SubscribeTracks(c.pendingReceivedTracks)
		if err != nil {
			c.log.Errorf("client: error subscribe tracks %s ", err.Error())
		}

		c.pendingReceivedTracks = make([]SubscribeTrackRequest, 0)
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

	if c.peerConnection.PC().RemoteDescription() == nil {
		c.pendingRemoteCandidates = append(c.pendingRemoteCandidates, candidate)
	} else {
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
	c.onIceCandidate = callback
}

func (c *Client) onIceCandidateCallback(candidate *webrtc.ICECandidate) {
	if c.onIceCandidate == nil {
		c.log.Infof("client: on ice candidate callback is not set")
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

	c.pendingLocalCandidates = c.pendingLocalCandidates[:0]
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
			BytesSent:      stat.OutboundRTPStreamStats.BytesSent,
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

func generateClientReceiverStats(c *Client, track IRemoteTrack, stat stats.Stats) (TrackReceivedStats, error) {
	bitrate, _ := c.stats.GetReceiverBitrate(track.ID(), track.RID())

	receivedStats := TrackReceivedStats{
		ID:              track.ID(),
		RID:             track.RID(),
		StreamID:        track.StreamID(),
		Kind:            track.Kind(),
		Codec:           track.Codec().MimeType,
		BytesReceived:   int64(stat.InboundRTPStreamStats.BytesReceived),
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
