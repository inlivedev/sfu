package sfu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
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
	Direction            webrtc.RTPTransceiverDirection
	IdleTimeout          time.Duration
	Type                 string
	EnableVoiceDetection bool
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
	internalDataChannel   *webrtc.DataChannel
	dataChannels          *DataChannelList
	estimator             cc.BandwidthEstimator
	initialTracksCount    atomic.Uint32
	isInRenegotiation     *atomic.Bool
	isInRemoteNegotiation *atomic.Bool
	IsSubscribeAllTracks  *atomic.Bool
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
	queue                             *queue
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
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		Direction:            webrtc.RTPTransceiverDirectionSendrecv,
		IdleTimeout:          30 * time.Second,
		Type:                 ClientTypePeer,
		EnableVoiceDetection: false,
	}
}

func NewClient(s *SFU, id string, name string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {
	var client *Client
	var vadInterceptor *voiceactivedetector.Interceptor

	localCtx, cancel := context.WithCancel(s.context)
	m := &webrtc.MediaEngine{}

	if err := RegisterCodecs(m, s.codecs); err != nil {
		panic(err)
	}

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
			vadInterceptor = i
		})

		i.Add(vadInterceptorFactory)
	}

	estimatorChan := make(chan cc.BandwidthEstimator, 1)

	if s.enableBandwidthEstimator {
		// Create a Congestion Controller. This analyzes inbound and outbound data and provides
		// suggestions on how much we should be sending.
		//
		// Passing `nil` means we use the default Estimation Algorithm which is Google Congestion Control.
		// You can use the other ones that Pion provides, or write your own!
		congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
			// if bw below 100_000, somehow the estimator will struggle to probe the bandwidth and will stuck there. So we set the min to 100_000
			// TODO: we need to use packet loss based bandwidth adjuster when the bandwidth is below 100_000
			return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(int(s.bitratesConfig.InitialBandwidth)))
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
	}

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
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
		canAddCandidate:                &atomic.Bool{},
		isInRenegotiation:              &atomic.Bool{},
		isInRemoteNegotiation:          &atomic.Bool{},
		IsSubscribeAllTracks:           &atomic.Bool{},
		dataChannels:                   NewDataChannelList(),
		mu:                             sync.Mutex{},
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
	}

	// setup internal data channel
	if opts.EnableVoiceDetection {
		client.enableSendVADToInternalDataChannel()
		client.enableVADStatUpdate()
	}

	client.quality.Store(QualityHigh)

	client.ingressQualityLimitationReason.Store("none")

	client.stats = newClientStats(client)

	client.bitrateController = newbitrateController(client, s.pliInterval)

	if s.enableBandwidthEstimator {
		go func() {
			estimator := <-estimatorChan
			client.estimator = estimator
		}()
	}

	peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		if client.isDebug {
			glog.Info("client: connection state changed ", connectionState.String())
		}
		client.onConnectionStateChanged(connectionState)
	})

	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		var track ITrack
		var vad *voiceactivedetector.VoiceDetector

		defer glog.Info("client: new track ", remoteTrack.ID(), " Kind:", remoteTrack.Kind(), " Codec: ", remoteTrack.Codec().MimeType, " RID: ", remoteTrack.RID())

		if remoteTrack.Kind() == webrtc.RTPCodecTypeAudio && client.IsVADEnabled() {
			vad = vadInterceptor.AddAudioTrack(remoteTrack)
		}

		onPLI := func() error {
			if client.peerConnection == nil || client.peerConnection.PC() == nil || client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
				return nil
			}

			return client.peerConnection.PC().WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{MediaSSRC: uint32(remoteTrack.SSRC())},
			})
		}

		onStatsUpdated := func(stats *stats.Stats) {
			client.mu.Lock()
			defer client.mu.Unlock()

			client.stats.SetReceiver(track.ID(), *stats)
			glog.Info("client: stats updated ", track.ID(), " ", stats)
		}

		if remoteTrack.RID() == "" {
			// not simulcast

			track = newTrack(client.context, client.id, remoteTrack, receiver, s.pliInterval, onPLI, vad, client.statsGetter, onStatsUpdated)

			track.OnEnded(func() {
				client.stats.removeReceiverStats(remoteTrack.ID())
			})

			if err := client.tracks.Add(track); err != nil {
				glog.Error("client: error add track ", err)
			}

			client.onTrack(track)
			track.SetAsProcessed()
		} else {
			// simulcast
			var simulcast *simulcastTrack
			var ok bool

			id := remoteTrack.ID()

			track, err = client.tracks.Get(id) // not found because the track is not added yet due to race condition

			if err != nil {
				// if track not found, add it
				track = newSimulcastTrack(client.context, client.id, remoteTrack, receiver, s.pliInterval, onPLI, client.statsGetter, onStatsUpdated)
				if err := client.tracks.Add(track); err != nil {
					glog.Error("client: error add track ", err)
				}

				track.OnEnded(func() {
					client.stats.removeReceiverStats(remoteTrack.ID())
				})
			} else if simulcast, ok = track.(*simulcastTrack); ok {
				simulcast.AddRemoteTrack(remoteTrack, receiver, client.statsGetter, onStatsUpdated)
			}

			// // only process track when the highest quality is available
			// simulcast.mu.Lock()
			// isHighAvailable := simulcast.remoteTrackHigh != nil
			// simulcast.mu.Unlock()

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

func (c *Client) OnTracksAdded(f func(addedTracks []ITrack)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onTracksAdded = f
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

	currentTransceiverCount := len(c.peerConnection.PC().GetTransceivers())

	// Set the remote SessionDescription
	err := c.peerConnection.PC().SetRemoteDescription(offer)
	if err != nil {
		return nil, err
	}

	// Create answer
	answer, err := c.peerConnection.PC().CreateAnswer(nil)
	if err != nil {
		return nil, err
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = c.peerConnection.PC().SetLocalDescription(answer)
	if err != nil {
		return nil, err
	}

	// allow add candidates once the local description is set
	c.canAddCandidate.Store(true)

	// process pending ice
	for _, iceCandidate := range c.pendingRemoteCandidates {
		err = c.peerConnection.PC().AddICECandidate(iceCandidate)
		if err != nil {
			panic(err)
		}
	}

	initialTrackCount := len(c.peerConnection.PC().GetTransceivers()) - currentTransceiverCount
	c.initialTracksCount.Store(uint32(initialTrackCount))

	// send pending local candidates if any
	c.sendPendingLocalCandidates()

	c.pendingRemoteCandidates = nil

	c.isInRemoteNegotiation.Store(false)

	// call renegotiation that might delay because the remote client is doing renegotiation

	return c.peerConnection.PC().LocalDescription(), nil
}

func (c *Client) renegotiate() {
	c.queue.Push(renegotiateQueue{
		Client: c,
	})
}

func (c *Client) OnBeforeRenegotiation(callback func(context.Context) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onBeforeRenegotiation = callback
}

func (c *Client) OnRenegotiation(callback func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onRenegotiation = callback
}

// TODO:
// delay negotiation using timeout and make sure only one negotiation is running when a client left and joined again
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
		glog.Info("sfu: renegotiation can't run, renegotiation still in progress ", c.ID)
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
			// mark negotiation is not needed after this done, so it will out of the loop
			c.negotiationNeeded.Store(false)

			// only renegotiate when client is connected
			if c.state.Load() != ClientStateEnded &&
				c.peerConnection.PC().SignalingState() == webrtc.SignalingStateStable &&
				c.peerConnection.PC().ConnectionState() == webrtc.PeerConnectionStateConnected {

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

	}()

}

func (c *Client) allowRemoteRenegotiation() {
	c.queue.Push(allowRemoteRenegotiationQueue{
		Client: c,
	})
}

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
		simulcastTrack := t.(*simulcastTrack)
		outputTrack = simulcastTrack.subscribe(c)

	} else {
		singleTrack := t.(*track)
		outputTrack = singleTrack.subscribe(c)
	}

	localTrack := outputTrack.LocalTrack()

	transc, err := c.peerConnection.PC().AddTransceiverFromTrack(localTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		glog.Error("client: error on adding track ", err)
		return nil
	}

	t.OnEnded(func() {
		if c == nil {
			return
		}

		c.mu.Lock()
		defer c.mu.Unlock()

		sender := transc.Sender()
		if sender == nil {
			return
		}

		if c.peerConnection == nil || c.peerConnection.PC() == nil || sender == nil || c.peerConnection.PC().ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}

		if err := c.peerConnection.PC().RemoveTrack(sender); err != nil {
			glog.Error("client: error remove track ", err)
			return
		}

		c.renegotiate()
	})

	// enable RTCP report and stats
	c.enableReportAndStats(transc.Sender(), outputTrack)

	return outputTrack
}

func (c *Client) enableReportAndStats(rtpSender *webrtc.RTPSender, track iClientTrack) {
	go func() {
		rtcpBuf := make([]byte, 1500)

		localCtx, cancel := context.WithCancel(c.context)

		defer cancel()

		for {
			select {
			case <-localCtx.Done():

				return
			default:
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}

			}
		}
	}()

	go func() {
		localCtx, cancel := context.WithCancel(c.context)
		defer cancel()
		for {
			select {
			case <-localCtx.Done():
				return
			default:
				rtcpPkts, _, err := rtpSender.ReadRTCP()
				if err != nil {
					return
				}

				for _, p := range rtcpPkts {
					switch p.(type) {
					case *rtcp.PictureLossIndication:
						track.RequestPLI()

						// case *rtcp.ReceiverEstimatedMaximumBitrate:

					}
				}
			}
		}
	}()

	go func() {
		localCtx, cancel := context.WithCancel(c.context)
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

func (c *Client) processPendingTracks() (isNeedNegotiation bool) {
	if len(c.pendingReceivedTracks) > 0 {
		err := c.SubscribeTracks(c.pendingReceivedTracks)
		if err != nil {
			glog.Error("client: error subscribe tracks ", err)
			return false
		}

		c.pendingReceivedTracks = make([]SubscribeTrackRequest, 0)

		isNeedNegotiation = true
	}

	return isNeedNegotiation
}

func (c *Client) afterClosed() {
	state := c.state.Load()
	if state != ClientStateEnded {
		c.state.Store(ClientStateEnded)
	}

	c.onLeft()

	c.cancel()

	c.sfu.onAfterClientStopped(c)
}

func (c *Client) stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.peerConnection.Close()
	if err != nil {
		return err
	}

	return nil
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

func (c *Client) OnConnectionStateChanged(callback func(webrtc.PeerConnectionState)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onConnectionStateChangedCallbacks = append(c.onConnectionStateChangedCallbacks, callback)
}

func (c *Client) onConnectionStateChanged(state webrtc.PeerConnectionState) {
	for _, callback := range c.onConnectionStateChangedCallbacks {
		callback(webrtc.PeerConnectionState(state))
	}
}

func (c *Client) onJoined() {
	for _, callback := range c.onJoinedCallbacks {
		callback()
	}
}

func (c *Client) OnJoined(callback func()) {
	c.onJoinedCallbacks = append(c.onJoinedCallbacks, callback)
}

func (c *Client) OnLeft(callback func()) {
	c.onLeftCallbacks = append(c.onLeftCallbacks, callback)
}

func (c *Client) onLeft() {
	for _, callback := range c.onLeftCallbacks {
		callback()
	}
}

func (c *Client) OnTrackRemoved(callback func(sourceType string, track *webrtc.TrackLocalStaticRTP)) {
	c.onTrackRemovedCallbacks = append(c.onTrackRemovedCallbacks, callback)
}

func (c *Client) IsBridge() bool {
	return c.Type() == ClientTypeUpBridge || c.Type() == ClientTypeDownBridge
}

func (c *Client) startIdleTimeout() {
	c.idleTimeoutContext, c.idleTimeoutCancel = context.WithTimeout(c.context, 5*time.Second)

	go func() {
		<-c.idleTimeoutContext.Done()
		glog.Info("client: idle timeout reached ", c.ID)
		c.afterClosed()
	}()
}

func (c *Client) cancelIdleTimeout() {
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
		c.sfu.onTracksAvailable(availableTracks)
	}
}

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
		c.renegotiate()

		// claim bitrates
		if err := c.bitrateController.addClaims(clientTracks); err != nil {
			glog.Error("sfu: failed to add claims ", err)
		}

		// request keyframe
		for _, track := range clientTracks {
			track.RequestPLI()
		}
	}

	return nil
}

func (c *Client) SubscribeAllTracks() {
	c.IsSubscribeAllTracks.Store(true)

	negotiateNeeded := c.sfu.syncTrack(c)

	if negotiateNeeded {
		c.renegotiate()
	}
}

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
			claim.track.(*simulcastClientTrack).remoteTrack.sendPLI(quality)
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
		estimated = uint32(c.sfu.bitratesConfig.InitialBandwidth)
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

	c.renegotiate()

	return newDc, nil
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
