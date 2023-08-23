package sfu

import (
	"context"
	"strconv"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
)

type SFU struct {
	clients                   map[string]*Client
	context                   context.Context
	cancel                    context.CancelFunc
	callbacksOnTrackPublished []func(map[string]*webrtc.TrackLocalStaticRTP)
	callbacksOnClientRemoved  []func(*Client)
	callbacksOnClientAdded    []func(*Client)
	Counter                   int
	publicDataChannels        map[string]map[string]*webrtc.DataChannel
	privateDataChannels       map[string]map[string]*webrtc.DataChannel
	idleChan                  chan bool
	mutex                     sync.RWMutex
	mux                       *UDPMux
	turnServer                TurnServer
	onStop                    func()
}

type TurnServer struct {
	Host     string
	Port     int
	Username string
	Password string
}

type PublishedTrack struct {
	ClientID string
	Track    webrtc.TrackLocal
}

func DefaultTurnServer() TurnServer {
	return TurnServer{
		Port:     3478,
		Host:     "turn.inlive.app",
		Username: "inlive",
		Password: "inlivesdkturn",
	}
}

// @Param muxPort: port for udp mux
func New(ctx context.Context, turnServer TurnServer, mux *UDPMux) *SFU {
	localCtx, cancel := context.WithCancel(ctx)

	sfu := &SFU{
		clients:             make(map[string]*Client),
		Counter:             0,
		context:             localCtx,
		cancel:              cancel,
		publicDataChannels:  make(map[string]map[string]*webrtc.DataChannel),
		privateDataChannels: make(map[string]map[string]*webrtc.DataChannel),
		mutex:               sync.RWMutex{},
		mux:                 mux,
		turnServer:          turnServer,
	}

	go func() {
	Out:
		for {
			select {
			case isIdle := <-sfu.idleChan:
				if isIdle {
					break Out
				}
			case <-sfu.context.Done():
				break Out
			}
		}

		cancel()
		sfu.Stop()
	}()

	return sfu
}

func (s *SFU) addClient(client *Client, direction webrtc.RTPTransceiverDirection) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, ok := s.clients[client.ID]; ok {
		panic("client already exists")
	}

	s.clients[client.ID] = client

	s.onClientAdded(client)
}

func (s *SFU) createClient(id string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {

	client := NewClient(s, id, peerConnectionConfig, opts)

	// Get the LocalDescription and take it to base64 so we can paste in browser
	return client
}

func (s *SFU) NewClient(id string, opts ClientOptions) *Client {
	s.Counter++

	// iceServers := []webrtc.ICEServer{{URLs: []string{
	// 	"stun:stun.l.google.com:19302",
	// }}}

	iceServers := []webrtc.ICEServer{}

	if s.turnServer.Host != "" {
		iceServers = append(iceServers,
			webrtc.ICEServer{
				URLs:           []string{"turn:" + s.turnServer.Host + ":" + strconv.Itoa(s.turnServer.Port)},
				Username:       s.turnServer.Username,
				Credential:     s.turnServer.Password,
				CredentialType: webrtc.ICECredentialTypePassword,
			},
			webrtc.ICEServer{
				URLs: []string{"stun:" + s.turnServer.Host + ":" + strconv.Itoa(s.turnServer.Port)},
			})
	}

	peerConnectionConfig := webrtc.Configuration{
		ICEServers: iceServers,
	}

	client := s.createClient(id, peerConnectionConfig, opts)

	client.OnConnectionStateChanged(func(connectionState webrtc.PeerConnectionState) {
		glog.Info("client: connection state changed ", client.ID, connectionState)
		switch connectionState {
		case webrtc.PeerConnectionStateConnected:
			if client.State == ClientStateNew {
				client.State = ClientStateActive
				client.onJoined()
			}

			needRenegotiation := false

			if len(client.pendingReceivedTracks) > 0 {
				client.processPendingTracks()

				needRenegotiation = true
			}

			if opts.Direction == webrtc.RTPTransceiverDirectionRecvonly || opts.Direction == webrtc.RTPTransceiverDirectionSendrecv {
				// get the tracks from other clients if the direction is receiving track
				isNeedRenegotiation := s.SyncTrack(client)
				if !needRenegotiation && isNeedRenegotiation {
					needRenegotiation = true
				}
			}

			if needRenegotiation {
				glog.Info("call renegotiate after sync ", client.ID)

				client.renegotiate()
			}

		case webrtc.PeerConnectionStateClosed:
			if client.State != ClientStateEnded {
				client.afterClosed()
			}
		case webrtc.PeerConnectionStateFailed:
			client.startIdleTimeout()
		case webrtc.PeerConnectionStateConnecting:
			client.cancelIdleTimeout()
		}
	})

	client.onTrack = func(ctx context.Context, localTrack *webrtc.TrackLocalStaticRTP) {
		client.mutex.Lock()
		defer client.mutex.Unlock()

		client.tracks[localTrack.ID()] = localTrack
		client.pendingPublishedTracks[localTrack.StreamID()+"-"+localTrack.ID()] = localTrack

		// don't publish track when not all the tracks are received
		// TODO:
		// 1. figure out how to handle this when doing a screensharing
		// 2. the renegotiation not triggered when new track after first negotiation is done
		if client.GetType() == ClientTypePeer && client.initialTracksCount > len(client.pendingPublishedTracks) {
			return
		}

		s.publishTracks(client.ID, client.pendingPublishedTracks)
		// reset pending published tracks after published
		client.pendingPublishedTracks = make(map[string]*webrtc.TrackLocalStaticRTP)

	}

	// request keyframe from new client for existing clients
	client.requestKeyFrame()

	client.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
		if state == webrtc.SignalingStateStable && client.pendingRemoteRenegotiation {
			client.pendingRemoteRenegotiation = false
			client.allowRemoteRenegotiation()
		}
	})

	s.addClient(client, client.GetDirection())

	return client
}

func (s *SFU) publishTracks(clientID string, tracks map[string]*webrtc.TrackLocalStaticRTP) {
	pendingTracks := []PublishedTrack{}

	s.mutex.Lock()

	for _, track := range tracks {
		// only publish track if it's not published yet

		newTrack := PublishedTrack{
			ClientID: clientID,
			Track:    track,
		}
		pendingTracks = append(pendingTracks, newTrack)

	}

	s.mutex.Unlock()

	s.broadcastTracks(pendingTracks)

	// request keyframe from existing client
	for _, client := range s.clients {
		client.requestKeyFrame()
	}

	s.onTrackPublished(tracks)
}

func (s *SFU) broadcastTracks(tracks []PublishedTrack) {
	for _, client := range s.clients {
		renegotiate := false

		for _, track := range tracks {
			if client.ID != track.ClientID {
				renegotiate = client.addTrack(track.Track.(*webrtc.TrackLocalStaticRTP))
			}
		}

		if renegotiate {
			glog.Info("sfu: call renegotiate after broadcast ", client.ID)
			client.renegotiate()
		}
	}
}

func (s *SFU) removeTrack(streamID, trackID string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	trackRemoved := false
	for _, client := range s.clients {
		clientRemoved := client.removePublishedTrack(streamID, trackID)
		if clientRemoved {
			trackRemoved = true
		}
	}

	return trackRemoved
}

// Syncs track from connected client to other clients
// returns true if need renegotiation
func (s *SFU) SyncTrack(client *Client) bool {
	currentTracks := client.GetCurrentTracks()

	needRenegotiation := false

	for _, clientPeer := range s.clients {
		for _, track := range clientPeer.tracks {
			if client.ID != clientPeer.ID {
				if _, ok := currentTracks[track.StreamID()+"-"+track.ID()]; !ok {
					client.addTrack(track)

					// request the keyframe from track publisher after added
					s.requestKeyFrameFromClient(clientPeer.ID)

					needRenegotiation = true
				}
			}
		}
	}

	return needRenegotiation
}

func (s *SFU) GetTracks() map[string]*webrtc.TrackLocalStaticRTP {
	tracks := make(map[string]*webrtc.TrackLocalStaticRTP)
	for _, client := range s.clients {
		for _, track := range client.tracks {
			tracks[track.StreamID()+"-"+track.ID()] = track
		}
	}

	return tracks
}

func (s *SFU) Stop() {
	for _, client := range s.clients {
		client.GetPeerConnection().Close()
	}

	if s.onStop != nil {
		s.onStop()
	}

	s.cancel()

}

func (s *SFU) OnStopped(callback func()) {
	s.onStop = callback
}

func (s *SFU) renegotiateAllClients() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, client := range s.clients {
		client.renegotiate()
	}
}

func (s *SFU) requestKeyFrameFromClient(clientID string) {
	if client, ok := s.clients[clientID]; ok {
		client.requestKeyFrame()
	}
}

func (s *SFU) OnTrackPublished(callback func(map[string]*webrtc.TrackLocalStaticRTP)) {
	s.callbacksOnTrackPublished = append(s.callbacksOnTrackPublished, callback)
}

func (s *SFU) OnClientAdded(callback func(*Client)) {
	s.callbacksOnClientAdded = append(s.callbacksOnClientAdded, callback)
}

func (s *SFU) OnClientRemoved(callback func(*Client)) {
	s.callbacksOnClientRemoved = append(s.callbacksOnClientRemoved, callback)
}

func (s *SFU) onAfterClientStopped(client *Client) {
	s.removeClient(client)
}

func (s *SFU) onClientAdded(client *Client) {
	for _, callback := range s.callbacksOnClientAdded {
		callback(client)
	}
}

func (s *SFU) onClientRemoved(client *Client) {
	for _, callback := range s.callbacksOnClientRemoved {
		callback(client)
	}
}

func (s *SFU) onTrackPublished(tracks map[string]*webrtc.TrackLocalStaticRTP) {
	for _, callback := range s.callbacksOnTrackPublished {
		callback(tracks)
	}
}

func (s *SFU) GetClients() map[string]*Client {
	return s.clients
}

func (s *SFU) GetClient(id string) (*Client, error) {
	if client, ok := s.clients[id]; ok {
		return client, nil
	}

	return nil, ErrClientNotFound
}

func (s *SFU) removeClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	delete(s.clients, client.ID)

	s.onClientRemoved(client)
}
