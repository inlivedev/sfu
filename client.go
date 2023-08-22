package sfu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

var (
	ClientStateNew     = 0
	ClientStateActive  = 1
	ClientStateRestart = 2
	ClientStateEnded   = 3

	ClientTypePeer       = "peer"
	ClientTypeUpBridge   = "upbridge"
	ClientTypeDownBridge = "downbridge"

	TrackTypeMedia  = "media"
	TrackTypeScreen = "screen"

	ErrNegotiationIsNotRequested = errors.New("client: error negotiation is called before requested")
	ErrClientStoped              = errors.New("client: error client already stopped")
)

type ClientOptions struct {
	Direction   webrtc.RTPTransceiverDirection
	IdleTimeout time.Duration
	Type        string
}

type ClientStats struct {
	Sender   map[webrtc.SSRC]stats.Stats
	Receiver map[webrtc.SSRC]stats.Stats
}

type Track struct {
	Track *webrtc.TrackLocalStaticRTP
	Type  string
}

type Client struct {
	ID                                string
	Context                           context.Context
	Cancel                            context.CancelFunc
	canAddCandidate                   bool
	clientStats                       ClientStats
	initialTracksCount                int
	isInRenegotiation                 bool
	isInRemoteNegotiation             bool
	idleTimeoutContext                context.Context
	idleTimeoutCancel                 context.CancelFunc
	mutex                             sync.Mutex
	peerConnection                    *webrtc.PeerConnection
	pendingReceivedTracks             map[string]*webrtc.TrackLocalStaticRTP
	pendingPublishedTracks            map[string]*webrtc.TrackLocalStaticRTP
	pendingRemoteRenegotiation        bool
	publishedTracks                   map[string]*webrtc.TrackLocalStaticRTP
	queue                             *queue
	State                             int
	sfu                               *SFU
	onConnectionStateChangedCallbacks []func(webrtc.PeerConnectionState)
	onJoinedCallbacks                 []func()
	onLeftCallbacks                   []func()
	onTrackAddedCallbacks             []func(sourceType string, track *webrtc.TrackLocalStaticRTP)
	onTrackRemovedCallbacks           []func(sourceType string, track *webrtc.TrackLocalStaticRTP)
	OnIceCandidate                    func(context.Context, *webrtc.ICECandidate)
	OnBeforeRenegotiation             func(context.Context) bool
	OnRenegotiation                   func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)
	OnAllowedRemoteRenegotiation      func()
	onTrack                           func(context.Context, *webrtc.TrackLocalStaticRTP)
	options                           ClientOptions
	statsGetter                       stats.Getter
	tracks                            map[string]*webrtc.TrackLocalStaticRTP
	NegotiationNeeded                 bool
	pendingRemoteCandidates           []webrtc.ICECandidateInit
	pendingLocalCandidates            []*webrtc.ICECandidate
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		Direction:   webrtc.RTPTransceiverDirectionSendrecv,
		IdleTimeout: 30 * time.Second,
		Type:        ClientTypePeer,
	}
}

func NewClient(s *SFU, id string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	// Enable simulcast in SDP
	for _, extension := range []string{
		"urn:ietf:params:rtp-hdrext:sdes:mid",
		"urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id",
		"urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id",
	} {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: extension}, webrtc.RTPCodecTypeVideo); err != nil {
			panic(err)
		}
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
		ID:      id,
		Context: localCtx,
		Cancel:  cancel,
		clientStats: ClientStats{
			Sender:   make(map[webrtc.SSRC]stats.Stats),
			Receiver: make(map[webrtc.SSRC]stats.Stats),
		},
		mutex:                  sync.Mutex{},
		peerConnection:         peerConnection,
		State:                  ClientStateNew,
		tracks:                 make(map[string]*webrtc.TrackLocalStaticRTP),
		options:                opts,
		pendingReceivedTracks:  make(map[string]*webrtc.TrackLocalStaticRTP),
		pendingPublishedTracks: make(map[string]*webrtc.TrackLocalStaticRTP),
		publishedTracks:        make(map[string]*webrtc.TrackLocalStaticRTP),
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
		trackCtx, trackCancel := context.WithCancel(client.Context)
		glog.Info("client: on track ", remoteTrack.ID(), remoteTrack.StreamID(), remoteTrack.Kind())

		// Create a local track, all our SFU clients will be fed via this track
		localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, remoteTrack.ID(), remoteTrack.StreamID())
		if newTrackErr != nil {
			panic(newTrackErr)
		}

		client.mutex.Lock()
		client.tracks[localTrack.ID()] = localTrack
		client.mutex.Unlock()

		rtpBuf := make([]byte, 1400)

		go func() {
			defer trackCancel()

			for {
				select {
				case <-trackCtx.Done():

					return
				default:
					i, _, readErr := remoteTrack.Read(rtpBuf)
					if readErr == io.EOF {
						client.removeTrack(localTrack.ID())
						needRenegotiate := s.removeTrack(localTrack.StreamID(), localTrack.ID())
						if needRenegotiate {
							s.renegotiateAllClients()
						}
						return
					}

					client.updateReceiverStats(remoteTrack)

					if readErr != nil {
						glog.Error("client: remote track read error ", readErr)
					}

					// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
					if _, err := localTrack.Write(rtpBuf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
						glog.Error("client: local track write error ", err)
					}
				}
			}
		}()

		client.onTrack(trackCtx, localTrack)
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

	return client
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
			answer, err := c.OnRenegotiation(c.Context, *c.peerConnection.LocalDescription())
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
func (c *Client) addTrack(track *webrtc.TrackLocalStaticRTP) bool {
	// if the client is not connected, we wait until it's connected in go routine
	if c.peerConnection.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
		c.mutex.Lock()
		c.pendingReceivedTracks[track.StreamID()+"-"+track.ID()] = track
		c.mutex.Unlock()

		return false
	}

	return c.setClientTrack(track)
}

func (c *Client) setClientTrack(track *webrtc.TrackLocalStaticRTP) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	id := track.StreamID() + "-" + track.ID()

	if _, ok := c.publishedTracks[id]; ok {
		glog.Warning("client: track already published ", c.ID, track.StreamID(), track.ID())
		return false
	}

	c.publishedTracks[id] = track

	rtpSender, err := c.peerConnection.AddTrack(track)
	if err != nil {
		glog.Error("client: error on adding track ", err)
		return false
	}

	go c.readRTCP(c.Context, rtpSender)

	return true
}

func (c *Client) removeTrack(trackID string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	delete(c.tracks, trackID)
}

// return boolean if a client need a renegotation because a track or more is removed
func (c *Client) removePublishedTrack(streamID, trackID string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	removed := false

	if _, ok := c.publishedTracks[fmt.Sprintf("%s-%s", streamID, trackID)]; ok {
		delete(c.publishedTracks, fmt.Sprintf("%s-%s", streamID, trackID))

		removed = true
	}

	if c.peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return false
	}

	for _, sender := range c.peerConnection.GetSenders() {
		track := sender.Track()
		if track != nil && track.ID() == trackID && track.StreamID() == streamID && c.peerConnection.ConnectionState() != webrtc.PeerConnectionStateClosed {
			if err := c.peerConnection.RemoveTrack(sender); err != nil {
				glog.Error("client: error remove track ", err)
			}
		}
	}

	return removed
}

func (c *Client) readRTCP(ctx context.Context, rtpSender *webrtc.RTPSender) {
	rtcpBuf := make([]byte, 1500)
	localCtx, cancel := context.WithCancel(ctx)

	defer cancel()

	for {
		select {
		case <-localCtx.Done():
			err := rtpSender.Stop()
			if err != nil {
				glog.Error("client: error stop rtp sender ", err)
			}

			return
		default:
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}

			c.updateSenderStats(rtpSender)
		}
	}
}

func (c *Client) processPendingTracks() bool {
	trackAdded := false

	for _, track := range c.pendingReceivedTracks {
		trackAdded = c.setClientTrack(track)
	}

	c.pendingReceivedTracks = make(map[string]*webrtc.TrackLocalStaticRTP)

	return trackAdded
}

func (c *Client) GetCurrentTracks() map[string]PublishedTrack {
	currentTracks := make(map[string]PublishedTrack)

	if c.peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed ||
		c.peerConnection.ConnectionState() == webrtc.PeerConnectionStateFailed {
		return currentTracks
	}

	for _, track := range c.publishedTracks {
		currentTracks[track.StreamID()+"-"+track.ID()] = PublishedTrack{
			ClientID: c.ID,
			Track:    track,
		}
	}

	return currentTracks
}

func (c *Client) afterClosed() {
	if c.State != ClientStateEnded {
		c.State = ClientStateEnded
	}
	// remove all tracks from client and SFU
	needRenegotiation := false
	for _, track := range c.tracks {
		needRenegotiation = c.sfu.removeTrack(track.StreamID(), track.ID())
	}

	// trigger renegotiation if needed to all existing clients
	if needRenegotiation {
		c.sfu.renegotiateAllClients()
	}

	c.Cancel()

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
		c.mutex.Lock()
		c.pendingRemoteCandidates = append(c.pendingRemoteCandidates, candidate)
		c.mutex.Unlock()
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

	c.OnIceCandidate(c.Context, candidate)
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

func (c *Client) OnTrackAdded(callback func(sourceType string, track *webrtc.TrackLocalStaticRTP)) {
	c.onTrackAddedCallbacks = append(c.onTrackAddedCallbacks, callback)
}

func (c *Client) OnTrackRemoved(callback func(sourceType string, track *webrtc.TrackLocalStaticRTP)) {
	c.onTrackRemovedCallbacks = append(c.onTrackRemovedCallbacks, callback)
}

func (c *Client) IsBridge() bool {
	return c.GetType() == ClientTypeUpBridge || c.GetType() == ClientTypeDownBridge
}

func (c *Client) startIdleTimeout() {
	c.idleTimeoutContext, c.idleTimeoutCancel = context.WithTimeout(c.Context, 30*time.Second)

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

func (c *Client) GetType() string {
	return c.options.Type
}

func (c *Client) GetDirection() webrtc.RTPTransceiverDirection {
	return c.options.Direction
}

func (c *Client) GetTracks() map[string]*webrtc.TrackLocalStaticRTP {
	return c.tracks
}

func (c *Client) GetPeerConnection() *webrtc.PeerConnection {
	return c.peerConnection
}

func (c *Client) GetStats() ClientStats {
	c.mutex.Lock()

	defer func() {
		c.mutex.Unlock()

	}()

	return c.clientStats
}

func (c *Client) updateReceiverStats(remoteTrack *webrtc.TrackRemote) {
	if c.statsGetter == nil {
		return
	}

	if remoteTrack == nil {
		return
	}

	if remoteTrack.SSRC() == 0 {
		return
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.clientStats.Receiver[remoteTrack.SSRC()] = *c.statsGetter.Get(uint32(remoteTrack.SSRC()))
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

	c.mutex.Lock()
	defer c.mutex.Unlock()

	ssrc := sender.GetParameters().Encodings[0].SSRC

	c.clientStats.Sender[ssrc] = *c.statsGetter.Get(uint32(ssrc))
}
