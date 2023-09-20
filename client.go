package sfu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/intervalpli"
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

	QualityHigh = 3
	QualityMid  = 2
	QualityLow  = 1
	QualityNone = 0

	lowBitrate           = 150_000
	midBitrate           = 500_000
	highBitrate          = 2_500_000
	initialSendBandwidth = 1_000_000
)

type QualityLevel int

var (
	ErrNegotiationIsNotRequested = errors.New("client: error negotiation is called before requested")
	ErrClientStoped              = errors.New("client: error client already stopped")
)

type ClientOptions struct {
	Direction                  webrtc.RTPTransceiverDirection
	IdleTimeout                time.Duration
	Type                       string
	EnableCongestionController bool
}

type ClientStats struct {
	mutex    sync.Mutex
	Sender   map[webrtc.SSRC]stats.Stats
	Receiver map[webrtc.SSRC]stats.Stats
}

type Client struct {
	id                                string
	context                           context.Context
	cancel                            context.CancelFunc
	canAddCandidate                   bool
	clientStats                       *ClientStats
	estimatorChan                     chan cc.BandwidthEstimator
	estimator                         cc.BandwidthEstimator
	initialTracksCount                int
	isInRenegotiation                 bool
	isInRemoteNegotiation             bool
	IsSubscribeAllTracks              bool
	idleTimeoutContext                context.Context
	idleTimeoutCancel                 context.CancelFunc
	mutex                             sync.Mutex
	peerConnection                    *webrtc.PeerConnection
	pendingReceivedTracks             *TrackList
	pendingPublishedTracks            *TrackList
	pendingRemoteRenegotiation        bool
	publishedTracks                   *TrackList
	queue                             *queue
	State                             ClientState
	sfu                               *SFU
	onConnectionStateChangedCallbacks []func(webrtc.PeerConnectionState)
	onJoinedCallbacks                 []func()
	onLeftCallbacks                   []func()
	onTrackRemovedCallbacks           []func(sourceType string, track *webrtc.TrackLocalStaticRTP)
	OnIceCandidate                    func(context.Context, *webrtc.ICECandidate)
	OnMinMaxBitrateAdjusted           func(context.Context, uint32, uint32)
	OnBeforeRenegotiation             func(context.Context) bool
	OnRenegotiation                   func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)
	OnAllowedRemoteRenegotiation      func()
	OnTracksAvailable                 func([]ITrack)
	// onTrack is used by SFU to take action when a new track is added to the client
	onTrack                 func(ITrack)
	OnTracksAdded           func([]ITrack)
	options                 ClientOptions
	statsGetter             stats.Getter
	tracks                  *TrackList
	NegotiationNeeded       bool
	pendingRemoteCandidates []webrtc.ICECandidateInit
	pendingLocalCandidates  []*webrtc.ICECandidate
	quality                 QualityLevel
	egressBandwidth         uint32
	ingressBandwidth        uint32
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		Direction:                  webrtc.RTPTransceiverDirectionSendrecv,
		IdleTimeout:                30 * time.Second,
		Type:                       ClientTypePeer,
		EnableCongestionController: true,
	}
}

func NewClient(s *SFU, id string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {
	m := &webrtc.MediaEngine{}

	if err := RegisterDefaultCodecs(m); err != nil {
		panic(err)
	}

	RegisterSimulcastHeaderExtensions(m, webrtc.RTPCodecTypeVideo)

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

	// add congestion control interceptor
	var congestionController *cc.InterceptorFactory

	var estimatorChan chan cc.BandwidthEstimator

	if opts.EnableCongestionController {
		glog.Info("client: enable congestion controller")
		congestionController, err = cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
			return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(initialSendBandwidth))
		})
		if err != nil {
			panic(err)
		}

		estimatorChan = make(chan cc.BandwidthEstimator, 1)
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

	// Register a intervalpli factory
	// This interceptor sends a PLI every 3 seconds. A PLI causes a video keyframe to be generated by the sender.
	// This makes our video seekable and more error resilent, but at a cost of lower picture quality and higher bitrates
	// A real world application should process incoming RTCP packets from viewers and forward them to senders
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}

	i.Add(intervalPliFactory)

	settingEngine := webrtc.SettingEngine{}

	if s.mux != nil {
		settingEngine.SetICEUDPMux(s.mux.mux)
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(i)).NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}

	s.setupDataChannelBroadcaster(peerConnection, id)

	// add other clients tracks before generate the answer
	// s.addOtherClientTracksBeforeSendAnswer(peerConnection)
	localCtx, cancel := context.WithCancel(s.context)

	client := &Client{
		id:            id,
		estimatorChan: estimatorChan,
		context:       localCtx,
		cancel:        cancel,
		clientStats: &ClientStats{
			mutex:    sync.Mutex{},
			Sender:   make(map[webrtc.SSRC]stats.Stats),
			Receiver: make(map[webrtc.SSRC]stats.Stats),
		},
		mutex:                  sync.Mutex{},
		egressBandwidth:        uint32(initialSendBandwidth),
		peerConnection:         peerConnection,
		State:                  ClientStateNew,
		tracks:                 newTrackList(),
		options:                opts,
		pendingReceivedTracks:  newTrackList(),
		pendingPublishedTracks: newTrackList(),
		publishedTracks:        newTrackList(),
		queue:                  NewQueue(localCtx),
		sfu:                    s,
		statsGetter:            statsGetter,
	}

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		glog.Info("client: ice connection state changed ", connectionState)
	})

	// TODOL: replace this with callback
	peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		client.onConnectionStateChanged(connectionState)
	})

	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		var track ITrack

		glog.Infof("client: ontrack %s %s %s", remoteTrack.Msid(), remoteTrack.Kind(), remoteTrack.RID())
		if remoteTrack.RID() == "" {
			// not simulcast
			track = NewTrack(client, remoteTrack)
			if err := client.tracks.Add(track); err != nil {
				glog.Error("client: error add track ", err)
			}
		} else {
			// simulcast
			id := remoteTrack.Msid()
			glog.Infof("client: simulcast track %s %s %s", id, remoteTrack.Kind(), remoteTrack.RID())
			track, err = client.tracks.Get(id) // not found because the track is not added yet due to race condition
			if err != nil {
				glog.Infof("client: track not found %s", id)
				// if track not found, add it
				track = NewSimulcastTrack(client, remoteTrack)
				if err := client.tracks.Add(track); err != nil {
					glog.Error("client: error add track ", err)
				}

				track.(*SimulcastTrack).OnTrackComplete(func() {
					glog.Info("client: track complete ", id)
				})
			} else if simulcast, ok := track.(*SimulcastTrack); ok {
				glog.Infof("client: track found %s", id)
				simulcast.AddRemoteTrack(remoteTrack)
			}

			// only add tracks after complete
			glog.Infof("client: total simulcast tracks %d", track.TotalTracks())
		}

		if !track.IsProcessed() {
			client.onTrack(track)
			track.SetAsProcessed()
		}

	})

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		// only sending candidate when the local description is set, means expecting the remote peer already has the remote description
		if candidate != nil {
			if client.canAddCandidate {
				go client.onIceCandidateCallback(candidate)

				return
			}

			client.pendingLocalCandidates = append(client.pendingLocalCandidates, candidate)
		}
	})

	if opts.EnableCongestionController {
		go func() {
			client.estimator = <-client.estimatorChan
		}()
	}

	return client
}

func (c *Client) ID() string {
	return c.id
}

func (c *Client) Context() context.Context {
	return c.context
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
	c.canAddCandidate = true

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
	if c.isInRenegotiation {
		c.pendingRemoteRenegotiation = true
		return false
	}

	c.isInRemoteNegotiation = true

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
	c.isInRemoteNegotiation = true

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
	c.canAddCandidate = true

	// process pending ice
	for _, iceCandidate := range c.pendingRemoteCandidates {
		err = c.peerConnection.AddICECandidate(iceCandidate)
		if err != nil {
			panic(err)
		}
	}

	c.initialTracksCount = len(c.peerConnection.GetTransceivers()) - currentTransceiverCount

	// send pending local candidates if any
	c.sendPendingLocalCandidates()

	c.pendingRemoteCandidates = nil

	c.isInRemoteNegotiation = false

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
	glog.Info("client: renegotiation started ", c.ID)
	if c.OnRenegotiation == nil {
		glog.Error("client: onRenegotiation is not set, can't do renegotiation")
		return
	}

	c.NegotiationNeeded = true

	if c.isInRemoteNegotiation {
		glog.Info("sfu: renegotiation is delayed because the remote client is doing negotiation ", c.ID)

		return
	}

	// no need to run another negotiation if it's already in progress, it will rerun because we mark the negotiationneeded to true
	if c.isInRenegotiation {
		glog.Info("sfu: renegotiation can't run, renegotiation still in progress ", c.ID)
		return
	}

	// mark negotiation is in progress to make sure no concurrent negotiation
	c.isInRenegotiation = true

	for c.NegotiationNeeded {
		// mark negotiation is not needed after this done, so it will out of the loop
		c.NegotiationNeeded = false

		// only renegotiate when client is connected
		if c.State != ClientStateEnded &&
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

	c.isInRenegotiation = false
}

func (c *Client) allowRemoteRenegotiation() {
	c.queue.Push(allowRemoteRenegotiationQueue{
		Client: c,
	})
}

// inform to remote client that it's allowed to do renegotiation through event
func (c *Client) allowRemoteRenegotiationQueuOp() {
	if c.OnAllowedRemoteRenegotiation != nil {
		c.isInRemoteNegotiation = true
		go c.OnAllowedRemoteRenegotiation()
	}
}

// return boolean if need a renegotiation after track added
func (c *Client) addTrack(track ITrack) bool {
	// if the client is not connected, we wait until it's connected in go routine
	if c.peerConnection.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
		if err := c.pendingReceivedTracks.Add(track); err != nil {
			glog.Error("client: error add pending received track ", err)
		}

		return false
	}

	return c.setClientTrack(track)
}

func (c *Client) setClientTrack(track ITrack) bool {
	var outputTrack webrtc.TrackLocal

	err := c.publishedTracks.Add(track)
	if err != nil {
		return false
	}

	if track.IsSimulcast() {
		simulcastTrack := track.(*SimulcastTrack)
		outputTrack = simulcastTrack.Subscribe(c)

	} else {
		singleTrack := track.(*Track)
		outputTrack = singleTrack.Subscribe()
	}

	transc, err := c.peerConnection.AddTransceiverFromTrack(outputTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		glog.Error("client: error on adding track ", err)
		return false
	}

	// enable RTCP report and stats
	c.enableReportAndStats(transc.Sender())

	return true
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

				c.publishedTracks.Remove(id)
			}
		}
	}

	if removed {
		c.renegotiate()
	}
}

func (c *Client) enableReportAndStats(rtpSender *webrtc.RTPSender) {
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
		tick := time.NewTicker(3 * time.Second)
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

func (c *Client) processPendingTracks() bool {
	trackAdded := false

	for _, track := range c.pendingReceivedTracks.tracks {

		isTrackAdded := c.setClientTrack(track)
		if isTrackAdded {
			trackAdded = true
		}
	}

	c.pendingReceivedTracks.Reset()

	return trackAdded
}

func (c *Client) GetCurrentTracks() map[string]ITrack {
	return c.publishedTracks.GetTracks()
}

func (c *Client) afterClosed() {
	if c.State != ClientStateEnded {
		c.State = ClientStateEnded
	}

	removeTrackIDs := make([]string, 0)

	for _, track := range c.tracks.GetTracks() {
		if track.IsSimulcast() {
			track = track.(*SimulcastTrack)
			removeTrackIDs = append(removeTrackIDs, track.RemoteTrack().track.ID())
		} else {
			removeTrackIDs = append(removeTrackIDs, track.(*Track).remoteTrack.track.ID())
		}
	}

	c.sfu.removeTracks(removeTrackIDs)

	c.cancel()

	c.sfu.onAfterClientStopped(c)
}

func (c *Client) Stop() error {
	if c.State == ClientStateEnded {
		return ErrClientStoped
	}

	c.State = ClientStateEnded

	err := c.peerConnection.Close()
	if err != nil {
		return err
	}

	c.afterClosed()

	return nil
}

func (c *Client) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if c.peerConnection.RemoteDescription() == nil {
		// c.mutex.Lock()
		c.pendingRemoteCandidates = append(c.pendingRemoteCandidates, candidate)
		// c.mutex.Unlock()
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
	for _, candidate := range c.pendingLocalCandidates {
		c.onIceCandidateCallback(candidate)
	}

	c.pendingLocalCandidates = nil
}

func (c *Client) requestKeyFrame() {
	for _, receiver := range c.peerConnection.GetReceivers() {
		if receiver.Track() == nil {
			continue
		}

		_ = c.peerConnection.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{
				MediaSSRC: uint32(receiver.Track().SSRC()),
			},
		})
	}
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
	c.idleTimeoutContext, c.idleTimeoutCancel = context.WithTimeout(c.context, 30*time.Second)

	go func() {
		<-c.idleTimeoutContext.Done()
		glog.Info("client: idle timeout reached ", c.ID)
		_ = c.Stop()
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

func (c *Client) Tracks() map[string]ITrack {
	return c.tracks.GetTracks()
}

func (c *Client) PeerConnection() *webrtc.PeerConnection {
	return c.peerConnection
}

func (c *Client) Stats() *ClientStats {
	return c.clientStats
}

func (c *Client) updateReceiverStats(track ITrack, remoteTrack *webrtc.TrackRemote) {
	if c.statsGetter == nil {
		return
	}

	if remoteTrack == nil {
		return
	}

	if remoteTrack.SSRC() == 0 {
		return
	}

	c.clientStats.mutex.Lock()
	defer c.clientStats.mutex.Unlock()

	c.clientStats.Receiver[remoteTrack.SSRC()] = *c.statsGetter.Get(uint32(remoteTrack.SSRC()))

	track.UpdateStats(remoteTrack, c.clientStats.Receiver[remoteTrack.SSRC()])
}

func (c *Client) updateSenderStats(sender *webrtc.RTPSender) {
	if c.statsGetter == nil {
		return
	}

	if sender == nil {
		return
	}

	if sender.Track() == nil {
		return
	}

	c.clientStats.mutex.Lock()
	defer c.clientStats.mutex.Unlock()

	ssrc := sender.GetParameters().Encodings[0].SSRC

	c.clientStats.Sender[ssrc] = *c.statsGetter.Get(uint32(ssrc))
}

func (c *Client) SetTracksSourceType(trackTypes map[string]TrackType) {
	availableTracks := make([]ITrack, 0)
	tracks := c.pendingPublishedTracks.GetTracks()
	for id, trackType := range trackTypes {
		if track, ok := tracks[id]; ok {
			track.SetSourceType(trackType)
			availableTracks = append(availableTracks, track)

			// remove it from pending published once it published available to other clients
			c.pendingPublishedTracks.Remove(track.ID())
		}
	}

	if len(availableTracks) > 0 {
		c.sfu.onTracksAvailable(availableTracks)
	}
}

func (c *Client) SubscribeTracks(req []SubscribeTrackRequest) error {
	negotiationNeeded := false

	for _, r := range req {
		trackFound := false

		// skip track if it's own track
		if c.ID() == r.ClientID {
			continue
		}

		if client, err := c.sfu.clients.GetClient(r.ClientID); err == nil {
			for _, track := range client.Tracks() {
				if track.ID() == r.TrackID {
					if c.addTrack(track) {
						negotiationNeeded = true
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

	if negotiationNeeded {
		c.renegotiate()
	}

	return nil
}

func (c *Client) SubscribeAllTracks() {
	c.IsSubscribeAllTracks = true

	negotiateNeeded := c.sfu.SyncTrack(c)

	if negotiateNeeded {
		c.renegotiate()
	}
}

func (c *Client) SetQuality(quality QualityLevel) {
	if c.quality == quality {
		return
	}

	glog.Infof("client: %s switch quality to %s", c.ID, quality)
	c.quality = quality
}

// This function is to calculate maximum bitrate allowed per audio or video track.
// The bitrate is calculated based on the estimated bandwidth and the number of tracks.
// Because we prioritize audio tracks, then the audio tracks will get the maximum bitrate allowed per track. TODO: this should be configurable
// The video tracks will get the rest of the bandwidth.
// TODO:
// - This should pass the track as parameter to it's maximum allowed bitrate, so we can check the priority of the track compare with the others.
// - For example if the track is speaking, then it should get the maximum bitrate allowed together with the other tracks in the same stream_id.
// - Check if the client has a manual overide quality, then use it instead of the estimated quality

// Currently the audio max bitrate has no affect, but we might able to control it with RTCP report
// return audio and video max bitrate allowed per track
func (c *Client) GetMaxBitratePerTrack() (uint32, uint32) {
	var bandwidthPerVideoTrack, bandwidthPerAudioTrack uint32

	// initial track count is set 1 considering there might be a new track without a bitrate date yet
	audioTracks := 1
	videoTracks := 1
	audioTotalBitrates := uint32(0)
	videoTotalBitrates := uint32(0)
	maxAudioBitrates := uint32(0)
	maxVideoBitrates := uint32(0)

	for _, track := range c.publishedTracks.GetTracks() {
		var currentBitrate uint32

		if track.Kind() == webrtc.RTPCodecTypeAudio {
			audioTracks++
			currentBitrate = track.RemoteTrack().GetCurrentBitrate()
			audioTotalBitrates += currentBitrate
			if currentBitrate > maxAudioBitrates {
				maxAudioBitrates = currentBitrate
			}

		}

		if track.Kind() == webrtc.RTPCodecTypeVideo {
			videoTracks++
			if track.IsSimulcast() {
				simulcastTracks := track.(*SimulcastTrack)

				for _, localTrack := range simulcastTracks.localTracks {
					switch localTrack.lastQuality {
					case QualityHigh:
						if simulcastTracks.remoteTrackHigh == nil {
							currentBitrate = localTrack.remoteTrack.remoteTrackHigh.GetCurrentBitrate()
						}
					case QualityMid:
						if simulcastTracks.remoteTrackMid != nil {
							currentBitrate = localTrack.remoteTrack.remoteTrackMid.GetCurrentBitrate()
						}
					case QualityLow:
						if simulcastTracks.remoteTrackLow != nil {
							currentBitrate = localTrack.remoteTrack.remoteTrackLow.GetCurrentBitrate()
						}
					case QualityNone:
						if simulcastTracks.remoteTrackLow != nil {
							currentBitrate = localTrack.remoteTrack.remoteTrackLow.GetCurrentBitrate()
							if currentBitrate == 0 {
								currentBitrate = MinBitrateUpperCap
							}
						}
					}

					videoTotalBitrates += currentBitrate
					if currentBitrate > maxVideoBitrates {
						maxVideoBitrates = currentBitrate
					}
				}

			} else {
				simpleTracks := track.(*Track)

				for _, localTrack := range simpleTracks.localTracks {
					currentBitrate = localTrack.remoteTrack.GetCurrentBitrate()
					if currentBitrate == 0 {
						currentBitrate = MinBitrateUpperCap
					}

					videoTotalBitrates += currentBitrate

					if currentBitrate > maxVideoBitrates {
						maxVideoBitrates = currentBitrate
					}
				}
			}
		}

	}

	if maxAudioBitrates < 24_000 {
		maxAudioBitrates = 48_000
	}

	bandwidth := c.GetEstimatedBandwidth()

	// make sure the max audio bitrate is not more than the estimated bandwidth
	if maxAudioBitrates*uint32(audioTracks) > bandwidth {
		maxAudioBitrates = bandwidth / uint32(audioTracks)
	}

	totalAudioBandwidth := maxAudioBitrates * uint32(audioTracks)

	if audioTracks == 0 {
		bandwidthPerAudioTrack = maxAudioBitrates
	} else {
		bandwidthPerAudioTrack = bandwidth / uint32(audioTracks)
	}

	if videoTracks == 0 {
		bandwidthPerVideoTrack = maxVideoBitrates
	} else {
		bandwidthPerVideoTrack = (bandwidth - totalAudioBandwidth) / uint32(videoTracks)
	}

	return bandwidthPerAudioTrack, bandwidthPerVideoTrack
}

func (c *Client) GetEstimatedBandwidth() uint32 {
	if c.estimator == nil {
		return uint32(initialSendBandwidth)
	}

	c.egressBandwidth = uint32(c.estimator.GetTargetBitrate())

	return c.egressBandwidth
}

func (c *Client) UpdatePublisherBandwidth(bitrate uint32) {
	if bitrate == 0 {
		return
	}

	c.ingressBandwidth = bitrate
}
