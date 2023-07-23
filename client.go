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

	ErrNegotiationIsNotRequested = errors.New("client: error negotiation is called before requested")
)

type Client struct {
	ID                                string
	Context                           context.Context
	Cancel                            context.CancelFunc
	canAddCandidate                   bool
	Direction                         webrtc.RTPTransceiverDirection
	intervalStarted                   bool
	initialTracksCount                int
	isInRenegotiation                 bool
	Type                              string
	InputChan                         chan interface{}
	mutex                             sync.RWMutex
	PeerConnection                    *webrtc.PeerConnection
	pendingReceivedTracks             map[string]*webrtc.TrackLocalStaticRTP
	pendingPublishedTracks            map[string]*webrtc.TrackLocalStaticRTP
	publishedTracks                   map[string]*webrtc.TrackLocalStaticRTP
	lastPLISent                       time.Time ``
	State                             int
	trackCount                        int
	OutputChan                        chan interface{}
	onConnectionStateChanged          func(webrtc.PeerConnectionState)
	onConnectionStateChangedCallbacks []func(webrtc.PeerConnectionState)
	OnIceCandidate                    func(context.Context, *webrtc.ICECandidate)
	OnBeforeRenegotiation             func(context.Context) bool
	OnRenegotiation                   func(context.Context, webrtc.SessionDescription) webrtc.SessionDescription
	onStopped                         func()
	onTrack                           func(context.Context, *webrtc.TrackLocalStaticRTP)
	Tracks                            map[string]*webrtc.TrackLocalStaticRTP
	NegotiationNeeded                 bool
	pendingRemoteCandidates           []webrtc.ICECandidateInit
	pendingLocalCandidates            []*webrtc.ICECandidate
}

// Init and Complete negotiation is used for bridging the room between servers
func (c *Client) InitNegotiation() *webrtc.SessionDescription {
	offer, err := c.PeerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	err = c.PeerConnection.SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	// allow add candidates once the local description is set
	c.canAddCandidate = true

	return c.PeerConnection.LocalDescription()
}

func (c *Client) CompleteNegotiation(answer webrtc.SessionDescription) {
	err := c.PeerConnection.SetRemoteDescription(answer)
	if err != nil {
		panic(err)
	}
}

// ask allow negotiation is required before call negotiation to make sure there is no racing condition of negotiation between local and remote clients.
// return false means the negotiation is in process, the requester must have a mechanism to repeat the request once it's done.
func (c *Client) IsAllowNegotiation() bool {
	if c.isInRenegotiation {
		return false
	}

	c.isInRenegotiation = true

	return true
}

func (c *Client) Negotiate(offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	// Set the remote SessionDescription
	err := c.PeerConnection.SetRemoteDescription(offer)
	if err != nil {
		return nil, err
	}

	// Create answer
	answer, err := c.PeerConnection.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = c.PeerConnection.SetLocalDescription(answer)
	if err != nil {
		return nil, err
	}

	// allow add candidates once the local description is set
	c.canAddCandidate = true

	// process pending ice
	for _, iceCandidate := range c.pendingRemoteCandidates {
		err = c.PeerConnection.AddICECandidate(iceCandidate)
		if err != nil {
			panic(err)
		}
	}

	c.initialTracksCount = len(c.PeerConnection.GetTransceivers())

	c.pendingRemoteCandidates = nil

	c.isInRenegotiation = false

	return c.PeerConnection.LocalDescription(), nil
}

// The renegotiation can be in race condition when a client is renegotiating and new track is added to the client because another client is publishing to the room.
// We can block the renegotiation until the current renegotiation is finish, but it will block the negotiation process for a while.
func (c *Client) renegotiate() error {
	if c.Type == ClientTypeUpBridge && c.OnBeforeRenegotiation != nil && !c.OnBeforeRenegotiation(c.Context) {
		log.Println("sfu: renegotiation is not allowed because the downbridge is doing renegotiation", c.ID)

		return nil
	}

	c.NegotiationNeeded = true

	// no need to run another negotiation if it's already in progress, it will rerun because we mark the negotiationneeded to true
	if c.isInRenegotiation {
		return nil
	}

	// mark negotiation is in progress to make sure no concurrent negotiation
	c.isInRenegotiation = true

	for c.NegotiationNeeded {
		// mark negotiation is not needed after this done, so it will out of the loop
		c.NegotiationNeeded = false

		// only renegotiate when client is connected
		if c.State != ClientStateEnded &&
			c.PeerConnection.SignalingState() == webrtc.SignalingStateStable &&
			c.PeerConnection.ConnectionState() == webrtc.PeerConnectionStateConnected {
			offer, err := c.PeerConnection.CreateOffer(nil)
			if err != nil {
				log.Println("sfu: error create offer on renegotiation ", err)
				return err
			}

			// Sets the LocalDescription, and starts our UDP listeners
			err = c.PeerConnection.SetLocalDescription(offer)
			if err != nil {
				log.Println("sfu: error set local description on renegotiation ", err)
				return err
			}

			// this will be blocking until the renegotiation is done
			answer := c.OnRenegotiation(c.Context, *c.PeerConnection.LocalDescription())

			if answer.Type != webrtc.SDPTypeAnswer {
				return errors.New("sfu: invalid answer type")
			}

			err = c.PeerConnection.SetRemoteDescription(answer)
			if err != nil {
				return err
			}
		}

		// if c.NegotiationNeeded {
		// 	log.Println("need renegotiate ", c.ID)
		// }
	}

	c.isInRenegotiation = false

	return nil
}

// return boolean if need a renegotiation after track added
func (c *Client) addTrack(track *webrtc.TrackLocalStaticRTP) bool {
	// if the client is not connected, we wait until it's connected in go routine
	if c.PeerConnection.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
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

	c.publishedTracks[track.StreamID()+"-"+track.ID()] = track

	rtpSender, err := c.PeerConnection.AddTrack(track)
	if err != nil {
		log.Println("client: error on adding track ", err)
		return false
	}

	go readRTCP(c.Context, rtpSender)

	return true
}

func (c *Client) removeTrack(trackID string) {
	if _, ok := c.Tracks[trackID]; ok {
		delete(c.Tracks, trackID)
	}
}

func (c *Client) removePublishedTrack(streamID, trackID string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	removed := false

	if _, ok := c.publishedTracks[fmt.Sprintf("%s-%s", streamID, trackID)]; ok {
		delete(c.publishedTracks, fmt.Sprintf("%s-%s", streamID, trackID))
		removed = true
	}

	if c.PeerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return false
	}

	for _, sender := range c.PeerConnection.GetSenders() {
		track := sender.Track()
		if track != nil && track.ID() == trackID && track.StreamID() == streamID {
			if err := c.PeerConnection.RemoveTrack(sender); err != nil {
				log.Println("client: error remove track ", err)
			} else {
				log.Println("client: published track removed ", trackID)
				go c.renegotiate()
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

	c.pendingReceivedTracks = nil

	return trackAdded
}

func (c *Client) GetCurrentTracks() map[string]PublishedTrack {
	currentTracks := make(map[string]PublishedTrack)

	if c.PeerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed ||
		c.PeerConnection.ConnectionState() == webrtc.PeerConnectionStateFailed {
		return currentTracks
	}

	for _, track := range c.Tracks {
		currentTracks[track.StreamID()+"-"+track.ID()] = PublishedTrack{
			ClientID: c.ID,
			Track:    track,
		}
	}

	return currentTracks
}

// return true if a RTPSender is stopped which will need a renegotiation for all subscribed clients
func (c *Client) afterClosed() {
	c.State = ClientStateEnded

	c.Cancel()

	if c.onStopped != nil {
		c.onStopped()
	}
}

func (c *Client) Stop() {
	c.PeerConnection.Close()
}

func (c *Client) OnStopped(callback func()) {
	c.onStopped = callback
}

func (c *Client) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if c.PeerConnection.RemoteDescription() == nil {
		c.mutex.Lock()
		c.pendingRemoteCandidates = append(c.pendingRemoteCandidates, candidate)
		c.mutex.Unlock()
	} else {
		if err := c.PeerConnection.AddICECandidate(candidate); err != nil {
			log.Println("client: error add ice candidate ", err)
			return err
		}
	}

	return nil
}

func (c *Client) onIceCandidateCallback(candidate *webrtc.ICECandidate) {
	c.OnIceCandidate(c.Context, candidate)
}

func (c *Client) SendPendingLocalCandidates() {
	for _, candidate := range c.pendingLocalCandidates {
		c.onIceCandidateCallback(candidate)
	}

	c.pendingLocalCandidates = nil
}

func (c *Client) requestKeyFrame() {
	for _, receiver := range c.PeerConnection.GetReceivers() {
		if receiver.Track() == nil {
			continue
		}

		_ = c.PeerConnection.WriteRTCP([]rtcp.Packet{
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
	return c.Type == ClientTypeUpBridge || c.Type == ClientTypeDownBridge
}