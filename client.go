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
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
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

	messageTypeVideoSize = "video_size"
	messageTypeStats     = "stats"
)

type QualityLevel uint32

var (
	ErrNegotiationIsNotRequested = errors.New("client: error negotiation is called before requested")
	ErrClientStoped              = errors.New("client: error client already stopped")
)

type ClientOptions struct {
	Direction   webrtc.RTPTransceiverDirection
	IdleTimeout time.Duration
	Type        string
}

type SenderStats struct {
	Track webrtc.TrackLocal
	Stats stats.Stats
}

type ReceiverStats struct {
	Track *webrtc.TrackRemote
	Stats stats.Stats
}

type internalDataMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
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

type ClientStats struct {
	senderMu   sync.RWMutex
	receiverMu sync.RWMutex
	Client     *Client
	Sender     map[string]SenderStats
	Receiver   map[string]ReceiverStats
}

func (c *ClientStats) removeSenderStats(trackId string) {
	c.senderMu.Lock()
	defer c.senderMu.Unlock()

	delete(c.Sender, trackId)
}

func (c *ClientStats) removeReceiverStats(trackId string) {
	c.receiverMu.Lock()
	defer c.receiverMu.Unlock()

	delete(c.Receiver, trackId)
}

type Client struct {
	id                string
	name              string
	bitrateController *bitrateController
	context           context.Context
	cancel            context.CancelFunc
	canAddCandidate   *atomic.Bool

	dataChannels                      *DataChannelList
	estimatorChan                     chan cc.BandwidthEstimator
	estimator                         cc.BandwidthEstimator
	initialTracksCount                atomic.Uint32
	isInRenegotiation                 *atomic.Bool
	isInRemoteNegotiation             *atomic.Bool
	IsSubscribeAllTracks              *atomic.Bool
	idleTimeoutContext                context.Context
	idleTimeoutCancel                 context.CancelFunc
	mu                                sync.Mutex
	peerConnection                    *webrtc.PeerConnection
	pendingReceivedTracks             []SubscribeTrackRequest
	pendingPublishedTracks            *trackList
	pendingRemoteRenegotiation        *atomic.Bool
	publishedTracks                   *trackList
	queue                             *queue
	state                             *atomic.Value
	sfu                               *SFU
	onConnectionStateChangedCallbacks []func(webrtc.PeerConnectionState)
	onJoinedCallbacks                 []func()
	onLeftCallbacks                   []func()
	onTrackRemovedCallbacks           []func(sourceType string, track *webrtc.TrackLocalStaticRTP)
	OnIceCandidate                    func(context.Context, *webrtc.ICECandidate)
	OnBeforeRenegotiation             func(context.Context) bool
	OnRenegotiation                   func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)
	OnAllowedRemoteRenegotiation      func()
	OnTracksAvailable                 func([]ITrack)
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
		Direction:   webrtc.RTPTransceiverDirectionSendrecv,
		IdleTimeout: 30 * time.Second,
		Type:        ClientTypePeer,
	}
}

func NewClient(s *SFU, id string, name string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {
	m := &webrtc.MediaEngine{}

	if err := RegisterCodecs(m, s.codecs); err != nil {
		panic(err)
	}

	RegisterSimulcastHeaderExtensions(m, webrtc.RTPCodecTypeVideo)
	RegisterAudioLevelHeaderExtension(m)

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

	if err = webrtc.ConfigureTWCCHeaderExtensionSender(m, i); err != nil {
		panic(err)
	}

	var estimatorChan chan cc.BandwidthEstimator

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	// // Register a intervalpli factory
	// // This interceptor sends a PLI every 3 seconds. A PLI causes a video keyframe to be generated by the sender.
	// // This makes our video seekable and more error resilent, but at a cost of lower picture quality and higher bitrates
	// // A real world application should process incoming RTCP packets from viewers and forward them to senders

	// pliOpts := intervalpli.GeneratorInterval(s.pliInterval)
	// intervalPliFactory, err := intervalpli.NewReceiverInterceptor(pliOpts)
	// if err != nil {
	// 	panic(err)
	// }

	// i.Add(intervalPliFactory)

	settingEngine := webrtc.SettingEngine{}

	if s.mux != nil {
		settingEngine.SetICEUDPMux(s.mux.mux)
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(i)).NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}

	// add other clients tracks before generate the answer
	// s.addOtherClientTracksBeforeSendAnswer(peerConnection)
	localCtx, cancel := context.WithCancel(s.context)
	var stateNew atomic.Value
	stateNew.Store(ClientStateNew)

	var quality atomic.Uint32
	quality.Store(QualityHigh)
	client := &Client{
		id:                             id,
		name:                           name,
		estimatorChan:                  estimatorChan,
		context:                        localCtx,
		cancel:                         cancel,
		canAddCandidate:                &atomic.Bool{},
		isInRenegotiation:              &atomic.Bool{},
		isInRemoteNegotiation:          &atomic.Bool{},
		IsSubscribeAllTracks:           &atomic.Bool{},
		dataChannels:                   NewDataChannelList(),
		mu:                             sync.Mutex{},
		negotiationNeeded:              &atomic.Bool{},
		peerConnection:                 peerConnection,
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
	}

	client.ingressQualityLimitationReason.Store("none")

	client.stats = &ClientStats{
		senderMu:   sync.RWMutex{},
		receiverMu: sync.RWMutex{},
		Client:     client,
		Sender:     make(map[string]SenderStats),
		Receiver:   make(map[string]ReceiverStats),
	}

	client.bitrateController = newbitrateController(client, s.pliInterval)

	peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		client.onConnectionStateChanged(connectionState)
	})

	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		var track ITrack

		glog.Info("client: new track ", remoteTrack.ID(), " Kind:", remoteTrack.Kind(), " Codec: ", remoteTrack.Codec().MimeType, " RID: ", remoteTrack.RID())

		if remoteTrack.RID() == "" {
			// not simulcast
			track = newTrack(client, remoteTrack, receiver)
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

			client.mu.Lock()
			track, err = client.tracks.Get(id) // not found because the track is not added yet due to race condition

			if err != nil {
				// if track not found, add it
				track = newSimulcastTrack(client, remoteTrack, receiver)
				if err := client.tracks.Add(track); err != nil {
					glog.Error("client: error add track ", err)
				}

				simulcast = track.(*simulcastTrack)

			} else if simulcast, ok = track.(*simulcastTrack); ok {
				simulcast.AddRemoteTrack(remoteTrack, receiver)
			}

			client.mu.Unlock()

			// only process track when the highest quality is available
			simulcast.mu.Lock()
			isHighAvailable := simulcast.remoteTrackHigh != nil
			simulcast.mu.Unlock()

			if isHighAvailable && !track.IsProcessed() {
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
	offer, err := c.peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	err = c.peerConnection.SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	// allow add candidates once the local description is set
	c.canAddCandidate.Store(true)

	return c.peerConnection.LocalDescription()
}

func (c *Client) CompleteNegotiation(answer webrtc.SessionDescription) {
	err := c.peerConnection.SetRemoteDescription(answer)
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

	currentTransceiverCount := len(c.peerConnection.GetTransceivers())

	// Set the remote SessionDescription
	err := c.peerConnection.SetRemoteDescription(offer)
	if err != nil {
		return nil, err
	}

	// Create answer
	answer, err := c.peerConnection.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = c.peerConnection.SetLocalDescription(answer)
	if err != nil {
		return nil, err
	}

	// allow add candidates once the local description is set
	c.canAddCandidate.Store(true)

	// process pending ice
	for _, iceCandidate := range c.pendingRemoteCandidates {
		err = c.peerConnection.AddICECandidate(iceCandidate)
		if err != nil {
			panic(err)
		}
	}

	initialTrackCount := len(c.peerConnection.GetTransceivers()) - currentTransceiverCount
	c.initialTracksCount.Store(uint32(initialTrackCount))

	// send pending local candidates if any
	c.sendPendingLocalCandidates()

	c.pendingRemoteCandidates = nil

	c.isInRemoteNegotiation.Store(false)

	// call renegotiation that might delay because the remote client is doing renegotiation

	return c.peerConnection.LocalDescription(), nil
}

func (c *Client) renegotiate() {
	c.queue.Push(renegotiateQueue{
		Client: c,
	})
}

// The renegotiation can be in race condition when a client is renegotiating and new track is added to the client because another client is publishing to the room.
// We can block the renegotiation until the current renegotiation is finish, but it will block the negotiation process for a while.
func (c *Client) renegotiateQueuOp() {

	if c.OnRenegotiation == nil {
		glog.Error("client: onRenegotiation is not set, can't do renegotiation")
		return
	}

	c.negotiationNeeded.Store(true)

	if c.isInRemoteNegotiation.Load() {
		glog.Info("sfu: renegotiation is delayed because the remote client is doing negotiation ", c.ID)

		return
	}

	// no need to run another negotiation if it's already in progress, it will rerun because we mark the negotiationneeded to true
	if c.isInRenegotiation.Load() {
		glog.Info("sfu: renegotiation can't run, renegotiation still in progress ", c.ID)
		return
	}

	// mark negotiation is in progress to make sure no concurrent negotiation
	c.isInRenegotiation.Store(true)

	for c.negotiationNeeded.Load() {
		// mark negotiation is not needed after this done, so it will out of the loop
		c.negotiationNeeded.Store(false)

		// only renegotiate when client is connected
		if c.state.Load() != ClientStateEnded &&
			c.peerConnection.SignalingState() == webrtc.SignalingStateStable &&
			c.peerConnection.ConnectionState() == webrtc.PeerConnectionStateConnected {

			offer, err := c.peerConnection.CreateOffer(nil)
			if err != nil {
				glog.Error("sfu: error create offer on renegotiation ", err)
				return
			}

			// Sets the LocalDescription, and starts our UDP listeners
			err = c.peerConnection.SetLocalDescription(offer)
			if err != nil {
				glog.Error("sfu: error set local description on renegotiation ", err)
				return
			}

			// this will be blocking until the renegotiation is done
			answer, err := c.OnRenegotiation(c.context, *c.peerConnection.LocalDescription())
			if err != nil {
				//TODO: when this happen, we need to close the client and ask the remote client to reconnect
				glog.Error("sfu: error on renegotiation ", err)
				return
			}

			if answer.Type != webrtc.SDPTypeAnswer {
				glog.Error("sfu: error on renegotiation, the answer is not an answer type")
				return
			}

			err = c.peerConnection.SetRemoteDescription(answer)
			if err != nil {
				return
			}
		}
	}

	c.isInRenegotiation.Store(false)
}

func (c *Client) allowRemoteRenegotiation() {
	c.queue.Push(allowRemoteRenegotiationQueue{
		Client: c,
	})
}

// inform to remote client that it's allowed to do renegotiation through event
func (c *Client) allowRemoteRenegotiationQueuOp() {
	if c.OnAllowedRemoteRenegotiation != nil {
		c.isInRemoteNegotiation.Store(true)
		go c.OnAllowedRemoteRenegotiation()
	}
}

// return boolean if need a renegotiation after track added
// func (c *Client) addTrack(t ITrack, clientID string) iClientTrack {
// 	// if the client is not connected, we wait until it's connected in go routine
// 	if c.peerConnection.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
// 		if err := c.pendingReceivedTracks.Add(t); err != nil {
// 			glog.Error("client: error add pending received track ", err)
// 		}

// 		return nil
// 	}

// 	return c.setClientTrack(t)
// }

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

	transc, err := c.peerConnection.AddTransceiverFromTrack(localTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		glog.Error("client: error on adding track ", err)
		return nil
	}

	// enable RTCP report and stats
	c.enableReportAndStats(transc.Sender(), outputTrack)

	return outputTrack
}

func (c *Client) removePublishedTrack(trackIDs []string) {
	removed := false

	if c.peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return
	}

	for _, id := range trackIDs {
		for _, sender := range c.peerConnection.GetSenders() {
			track := sender.Track()
			if track != nil && track.ID() == id && c.peerConnection.ConnectionState() != webrtc.PeerConnectionStateClosed {
				if err := c.peerConnection.RemoveTrack(sender); err != nil {
					glog.Error("client: error remove track ", err)
				}
				removed = true

				c.publishedTracks.Remove([]string{id})
			}
		}
	}

	if removed {
		c.renegotiate()
	}
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
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.state.Load()
	if state != ClientStateEnded {
		c.state.Store(ClientStateEnded)
	}

	removeTrackIDs := make([]string, 0)

	for _, track := range c.tracks.GetTracks() {
		removeTrackIDs = append(removeTrackIDs, track.ID())
	}

	c.sfu.removeTracks(removeTrackIDs)

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
	if c.peerConnection.RemoteDescription() == nil {
		// c.mu.Lock()
		c.pendingRemoteCandidates = append(c.pendingRemoteCandidates, candidate)
		// c.mu.Unlock()
	} else {
		if err := c.peerConnection.AddICECandidate(candidate); err != nil {
			glog.Error("client: error add ice candidate ", err)
			return err
		}
	}

	return nil
}

func (c *Client) onIceCandidateCallback(candidate *webrtc.ICECandidate) {
	if c.OnIceCandidate == nil {
		glog.Info("client: on ice candidate callback is not set")
		return
	}

	c.OnIceCandidate(c.context, candidate)
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

func (c *Client) PeerConnection() *webrtc.PeerConnection {
	return c.peerConnection
}

func (c *Client) Stats() *ClientStats {
	return c.stats
}

func (c *Client) updateReceiverStats(remoteTrack *remoteTrack) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.statsGetter == nil {
		return
	}

	if remoteTrack.track == nil {
		return
	}

	track := remoteTrack.track

	if track.SSRC() == 0 {
		return
	}

	c.stats.receiverMu.Lock()
	defer c.stats.receiverMu.Unlock()

	stats := c.statsGetter.Get(uint32(track.SSRC()))
	if stats != nil {
		remoteTrack.setReceiverStats(*stats)
		c.stats.Receiver[track.ID()] = ReceiverStats{
			Stats: *stats,
			Track: track,
		}
	}

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

	c.stats.senderMu.Lock()
	defer c.stats.senderMu.Unlock()

	ssrc := sender.GetParameters().Encodings[0].SSRC

	stats := c.statsGetter.Get(uint32(ssrc))
	if stats != nil {
		c.stats.Sender[sender.Track().ID()] = SenderStats{
			Stats: *stats,
			Track: sender.Track(),
		}
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

	c.pendingPublishedTracks.Remove(removeTrackIDs)

	if len(availableTracks) > 0 {
		c.sfu.onTracksAvailable(availableTracks)
	}
}

func (c *Client) SubscribeTracks(req []SubscribeTrackRequest) error {
	if c.peerConnection.ConnectionState() != webrtc.PeerConnectionStateConnected {
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
	if c.quality.Load() == uint32(quality) {
		return
	}

	glog.Infof("client: %s switch quality to %s", c.ID, quality)
	c.quality.Store(uint32(quality))
}

// GetEstimatedBandwidth returns the estimated bandwidth in bits per second based on
// Google Congestion Controller estimation. If the congestion controller is not enabled,
// it will return the initial bandwidth. If the receiving bandwidth is not 0, it will return the smallest value between
// the estimated bandwidth and the receiving bandwidth.
func (c *Client) GetEstimatedBandwidth() uint32 {
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

	newDc, err := c.peerConnection.CreateDataChannel(label, initOpts)
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
	newDc, err := c.peerConnection.CreateDataChannel(label, &webrtc.DataChannelInit{Ordered: &ordered})
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

		glog.Info("client: video size changed ", internalData.Data.Width)

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

func (c *Client) TrackStats() *ClientTrackStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	clientStats := &ClientTrackStats{
		ID:                       c.id,
		Name:                     c.name,
		ConsumerBandwidth:        c.egressBandwidth.Load(),
		PublisherBandwidth:       c.ingressBandwidth.Load(),
		Sents:                    make([]TrackSentStats, 0),
		Receives:                 make([]TrackReceivedStats, 0),
		CurrentPublishLimitation: c.ingressQualityLimitationReason.Load().(string),
		CurrentConsumerBitrate:   c.bitrateController.totalSentBitrates(),
	}

	c.stats.receiverMu.Lock()
	for _, stat := range c.stats.Receiver {
		receivedStats := TrackReceivedStats{
			ID:             stat.Track.ID(),
			Kind:           stat.Track.Kind().String(),
			Codec:          stat.Track.Codec().MimeType,
			PacketsLost:    stat.Stats.InboundRTPStreamStats.PacketsLost,
			PacketReceived: stat.Stats.InboundRTPStreamStats.PacketsReceived,
		}

		clientStats.Receives = append(clientStats.Receives, receivedStats)
	}

	c.stats.receiverMu.Unlock()

	c.stats.senderMu.Lock()
	for _, stat := range c.stats.Sender {
		claim := c.bitrateController.GetClaim(stat.Track.ID())
		source := "media"

		if claim.track == nil {
			continue
		}

		if claim.track.IsScreen() {
			source = "screen"
		}

		sentStats := TrackSentStats{
			ID:             stat.Track.ID(),
			Kind:           stat.Track.Kind().String(),
			PacketsLost:    stat.Stats.RemoteInboundRTPStreamStats.PacketsLost,
			PacketSent:     stat.Stats.OutboundRTPStreamStats.PacketsSent,
			FractionLost:   stat.Stats.RemoteInboundRTPStreamStats.FractionLost,
			ByteSent:       stat.Stats.OutboundRTPStreamStats.BytesSent,
			CurrentBitrate: uint64(claim.track.getCurrentBitrate()),
			Source:         source,
			ClaimedBitrate: uint64(claim.bitrate),
			Quality:        claim.quality,
		}

		clientStats.Sents = append(clientStats.Sents, sentStats)
	}

	c.stats.senderMu.Unlock()

	return clientStats
}

func (c *Client) EnableDebug() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.isDebug = true
}

func (c *Client) IsDebugEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.isDebug
}
