package sfu

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

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

type Track struct {
	Track *webrtc.TrackLocalStaticRTP
	Type  string
}

type Client struct {
	ID                                string
	Context                           context.Context
	Cancel                            context.CancelFunc
	canAddCandidate                   bool
	initialTracksCount                int
	isInRenegotiation                 bool
	isInRemoteNegotiation             bool
	idleTimeoutContext                context.Context
	idleTimeoutCancel                 context.CancelFunc
	mutex                             sync.RWMutex
	peerConnection                    *webrtc.PeerConnection
	pendingReceivedTracks             map[string]*webrtc.TrackLocalStaticRTP
	pendingPublishedTracks            map[string]*webrtc.TrackLocalStaticRTP
	pendingRemoteRenegotiation        bool
	publishedTracks                   map[string]*webrtc.TrackLocalStaticRTP
	queue                             *queue
	State                             int
	sfu                               *SFU
	onConnectionStateChanged          func(webrtc.PeerConnectionState)
	onConnectionStateChangedCallbacks []func(webrtc.PeerConnectionState)
	OnIceCandidate                    func(context.Context, *webrtc.ICECandidate)
	OnBeforeRenegotiation             func(context.Context) bool
	OnRenegotiation                   func(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error)
	OnAllowedRemoteRenegotiation      func()
	onStopped                         func()
	onTrack                           func(context.Context, *webrtc.TrackLocalStaticRTP)
	options                           ClientOptions
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
	c.NegotiationNeeded = true

	if c.isInRemoteNegotiation {
		log.Println("sfu: renegotiation is delayed because the remote client is doing negotiation", c.ID)

		return
	}

	// no need to run another negotiation if it's already in progress, it will rerun because we mark the negotiationneeded to true
	if c.isInRenegotiation {
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
				log.Println("sfu: error create offer on renegotiation ", err)
				return
			}

			// Sets the LocalDescription, and starts our UDP listeners
			err = c.peerConnection.SetLocalDescription(offer)
			if err != nil {
				log.Println("sfu: error set local description on renegotiation ", err)
				return
			}

			// this will be blocking until the renegotiation is done
			answer, err := c.OnRenegotiation(c.Context, *c.peerConnection.LocalDescription())
			if err != nil {
				//TODO: when this happen, we need to close the client and ask the remote client to reconnect
				log.Println("sfu: error on renegotiation ", err)
				return
			}

			if answer.Type != webrtc.SDPTypeAnswer {
				log.Println("sfu: error on renegotiation, the answer is not an answer type")
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
		log.Println("client: track already published ", c.ID, track.StreamID(), track.ID())
		return false
	}

	c.publishedTracks[id] = track

	rtpSender, err := c.peerConnection.AddTrack(track)
	if err != nil {
		log.Println("client: error on adding track ", err)
		return false
	}

	go readRTCP(c.Context, rtpSender)

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
		if track != nil && track.ID() == trackID && track.StreamID() == streamID {
			if err := c.peerConnection.RemoveTrack(sender); err != nil {
				log.Println("client: error remove track ", err)
			}
		}
	}

	return removed
}

func readRTCP(ctx context.Context, rtpSender *webrtc.RTPSender) {
	rtcpBuf := make([]byte, 1500)
	localCtx, cancel := context.WithCancel(ctx)

	defer cancel()

	for {
		select {
		case <-localCtx.Done():
			err := rtpSender.Stop()
			if err != nil {
				log.Println("client: error stop rtp sender ", err)
			}

			return
		default:
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
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
	c.State = ClientStateEnded

	// remove all tracks from client and SFU
	needRenegotiation := false
	for _, track := range c.tracks {
		needRenegotiation = c.sfu.removeTrack(track.StreamID(), track.ID())
	}

	// trigger renegotiation if needed to all existing clients
	if needRenegotiation {
		c.sfu.renegotiateAllClients()
	}

	c.sfu.onAfterClientStopped(c)

	c.Cancel()

	if c.onStopped != nil {
		c.onStopped()
	}
}

func (c *Client) Stop() error {
	if c.State == ClientStateEnded {
		return ErrClientStoped
	}

	err := c.peerConnection.Close()
	if err != nil {
		return err
	}

	// this will trigger remove client from SFU
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
			log.Println("client: error add ice candidate ", err)
			return err
		}
	}

	return nil
}

func (c *Client) onIceCandidateCallback(candidate *webrtc.ICECandidate) {
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

func (c *Client) IsBridge() bool {
	return c.GetType() == ClientTypeUpBridge || c.GetType() == ClientTypeDownBridge
}

func (c *Client) startIdleTimeout() {
	c.idleTimeoutContext, c.idleTimeoutCancel = context.WithTimeout(c.Context, 30*time.Second)

	go func() {
		<-c.idleTimeoutContext.Done()
		log.Println("client: idle timeout reached ", c.ID)
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

func (c *Client) errorHandler(err error) {
	log.Println("client: error ", err)
}
