package sfu

import (
	"context"
	"strconv"
	"sync"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
)

type SFU struct {
	clients                  map[string]*Client
	context                  context.Context
	cancel                   context.CancelFunc
	callbacksOnClientRemoved []func(*Client)
	callbacksOnClientAdded   []func(*Client)
	Counter                  int
	publicDataChannels       map[string]map[string]*webrtc.DataChannel
	privateDataChannels      map[string]map[string]*webrtc.DataChannel
	idleChan                 chan bool
	mutex                    sync.RWMutex
	mux                      *UDPMux
	turnServer               TurnServer
	onStop                   func()
	OnTracksAvailable        func(tracks []Track)
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

func (s *SFU) addClient(client *Client) {
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

			if client.pendingReceivedTracks.Length() > 0 {
				client.processPendingTracks()

				needRenegotiation = true
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

	client.onTrack = func(ctx context.Context, track *Track) {
		client.mutex.Lock()
		defer client.mutex.Unlock()
		if err := client.pendingPublishedTracks.Add(track); err != nil {
			glog.Error("client: failed to add pending published track ", err)
			return
		}

		// don't publish track when not all the tracks are received
		// TODO:
		// 1. figure out how to handle this when doing a screensharing
		// 2. the renegotiation not triggered when new track after negotiation is done
		if client.GetType() == ClientTypePeer && client.initialTracksCount > client.pendingPublishedTracks.Length() {
			return
		}

		availableTracks := make([]*Track, 0)
		for _, track := range client.pendingPublishedTracks.GetTracks() {
			availableTracks = append(availableTracks, track)
		}

		if client.OnTracksAdded != nil {
			client.OnTracksAdded(availableTracks)
		}

		// broadcast to client with auto subscribe tracks
		s.broadcastTracksToAutoSubscribeClients(availableTracks)

		// reset pending published tracks after published
		client.pendingPublishedTracks.Reset()

	}

	// request keyframe from new client for existing clients
	client.requestKeyFrame()

	client.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
		if state == webrtc.SignalingStateStable && client.pendingRemoteRenegotiation {
			client.pendingRemoteRenegotiation = false
			client.allowRemoteRenegotiation()
		}
	})

	s.addClient(client)

	return client
}

func (s *SFU) removeTracks(tracks []*Track) {
	for _, client := range s.clients {
		client.removePublishedTrack(tracks)
	}
}

// Syncs track from connected client to other clients
// returns true if need renegotiation
func (s *SFU) SyncTrack(client *Client) bool {
	currentTracks := client.GetCurrentTracks()

	needRenegotiation := false

	for _, clientPeer := range s.clients {
		for _, track := range clientPeer.tracks.GetTracks() {
			if client.ID != clientPeer.ID {
				if _, ok := currentTracks[track.LocalStaticRTP.StreamID()+"-"+track.LocalStaticRTP.ID()]; !ok {
					isNeedRenegotiation := client.addTrack(track)

					// request the keyframe from track publisher after added
					s.requestKeyFrameFromClient(clientPeer.ID)
					if isNeedRenegotiation {
						needRenegotiation = true
					}
				}
			}
		}
	}

	return needRenegotiation
}

func (s *SFU) GetTracks() []*Track {
	tracks := make([]*Track, 0)
	for _, client := range s.clients {
		for _, track := range client.tracks.GetTracks() {
			tracks = append(tracks, track)
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

func (s *SFU) requestKeyFrameFromClient(clientID string) {
	if client, ok := s.clients[clientID]; ok {
		client.requestKeyFrame()
	}
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

func (s *SFU) onTracksAvailable(tracks []*Track) {
	for _, client := range s.clients {
		if !client.IsSubscribeAllTracks && client.OnTracksAvailable != nil {
			client.OnTracksAvailable(tracks)
		}
	}
}

func (s *SFU) broadcastTracksToAutoSubscribeClients(tracks []*Track) {
	trackReq := make([]SubscribeTrackRequest, 0)
	for _, track := range tracks {
		trackReq = append(trackReq, SubscribeTrackRequest{
			ClientID: track.ClientID,
			StreamID: track.LocalStaticRTP.StreamID(),
			TrackID:  track.LocalStaticRTP.ID(),
		})
	}

	for _, client := range s.clients {
		if client.IsSubscribeAllTracks {
			if err := client.SubscribeTracks(trackReq); err != nil {
				glog.Error("client: failed to subscribe tracks ", err)
			}
		}
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
