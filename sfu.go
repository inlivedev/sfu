package sfu

import (
	"context"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
	"golang.org/x/exp/slices"
)

const (
	MaxBitrateUpperCap = 4_000_000
	MinBitrateUpperCap = 300_000
	MinBitrateLowerCap = 100_000
)

type SFUClients struct {
	clients map[string]*Client
	mutex   sync.Mutex
}

func (s *SFUClients) GetClients() map[string]*Client {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	clients := make(map[string]*Client)
	for k, v := range s.clients {
		clients[k] = v
	}

	return clients
}

func (s *SFUClients) GetClient(id string) (*Client, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if client, ok := s.clients[id]; ok {
		return client, nil
	}

	return nil, ErrClientNotFound
}

func (s *SFUClients) Length() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return len(s.clients)
}

func (s *SFUClients) Add(client *Client) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, ok := s.clients[client.ID()]; ok {
		return ErrClientExists
	}

	s.clients[client.ID()] = client

	return nil
}

func (s *SFUClients) Remove(client *Client) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, ok := s.clients[client.ID()]; !ok {
		return ErrClientNotFound
	}

	delete(s.clients, client.ID())

	return nil
}

type SFU struct {
	clients                  *SFUClients
	context                  context.Context
	cancel                   context.CancelFunc
	callbacksOnClientRemoved []func(*Client)
	callbacksOnClientAdded   []func(*Client)
	Counter                  int
	dataChannels             *SFUDataChannelList
	idleChan                 chan bool
	iceServers               []webrtc.ICEServer
	mutex                    sync.Mutex
	mux                      *UDPMux
	onStop                   func()
	OnTracksAvailable        func(tracks []Track)
}

type PublishedTrack struct {
	ClientID string
	Track    webrtc.TrackLocal
}

// @Param muxPort: port for udp mux
func New(ctx context.Context, iceServers []webrtc.ICEServer, mux *UDPMux) *SFU {
	localCtx, cancel := context.WithCancel(ctx)

	sfu := &SFU{
		clients:      &SFUClients{clients: make(map[string]*Client), mutex: sync.Mutex{}},
		Counter:      0,
		context:      localCtx,
		cancel:       cancel,
		dataChannels: NewSFUDataChannelList(),
		mutex:        sync.Mutex{},
		iceServers:   iceServers,
		mux:          mux,
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

	sfu.monitorAndAdjustBandwidth()

	return sfu
}

func (s *SFU) addClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if err := s.clients.Add(client); err != nil {
		glog.Error("sfu: failed to add client ", err)
		return
	}

	s.onClientAdded(client)
}

func (s *SFU) createClient(id string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {

	client := NewClient(s, id, peerConnectionConfig, opts)

	// Get the LocalDescription and take it to base64 so we can paste in browser
	return client
}

func (s *SFU) NewClient(id string, opts ClientOptions) *Client {
	s.Counter++

	peerConnectionConfig := webrtc.Configuration{
		ICEServers: s.iceServers,
	}

	client := s.createClient(id, peerConnectionConfig, opts)

	client.OnConnectionStateChanged(func(connectionState webrtc.PeerConnectionState) {
		glog.Info("client: connection state changed ", client.ID(), connectionState)
		switch connectionState {
		case webrtc.PeerConnectionStateConnected:
			if client.state.Load() == ClientStateNew {
				client.mu.Lock()
				defer client.mu.Unlock()
				client.state.Store(ClientStateActive)
				client.onJoined()

				// trigger available tracks from other clients
				if client.OnTracksAvailable != nil {
					availableTracks := make([]ITrack, 0)

					for _, c := range s.clients.GetClients() {
						for _, track := range c.tracks.GetTracks() {
							if track.Client().ID() != client.ID() {
								availableTracks = append(availableTracks, track)
							}
						}
					}

					if len(availableTracks) > 0 {
						client.OnTracksAvailable(availableTracks)
					}
				}
			}

			needRenegotiation := false

			if client.pendingReceivedTracks.Length() > 0 {
				client.processPendingTracks()

				needRenegotiation = true
			}

			if needRenegotiation {
				glog.Info("call renegotiate after sync ", client.ID())

				client.renegotiate()
			}

		case webrtc.PeerConnectionStateClosed:
			if client.state.Load() != ClientStateEnded {
				client.afterClosed()
			}
		case webrtc.PeerConnectionStateFailed:
			client.startIdleTimeout()
		case webrtc.PeerConnectionStateConnecting:
			client.cancelIdleTimeout()
		}
	})

	client.onTrack = func(track ITrack) {
		client.mu.Lock()
		defer client.mu.Unlock()

		if err := client.pendingPublishedTracks.Add(track); err == ErrTrackExists {
			// not an error could be because a simulcast track already added
			return
		}

		// don't publish track when not all the tracks are received
		// TODO:
		// 1. need to handle simulcast track because  it will be counted as single track
		if client.Type() == ClientTypePeer && int(client.initialTracksCount.Load()) > client.pendingPublishedTracks.Length() {
			return
		}

		glog.Info("publish tracks")

		availableTracks := client.pendingPublishedTracks.GetTracks()

		if client.onTracksAdded != nil {
			client.onTracksAdded(availableTracks)
		}

		// broadcast to client with auto subscribe tracks
		s.broadcastTracksToAutoSubscribeClients(availableTracks)
	}

	// request keyframe from new client for existing clients
	client.requestKeyFrame()

	client.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
		if state == webrtc.SignalingStateStable && client.pendingRemoteRenegotiation.Load() {
			client.pendingRemoteRenegotiation.Store(false)
			client.allowRemoteRenegotiation()
		}
	})

	s.addClient(client)

	return client
}

func (s *SFU) removeTracks(trackIDs []string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, client := range s.clients.GetClients() {
		client.removePublishedTrack(trackIDs)
	}
}

// Syncs track from connected client to other clients
// returns true if need renegotiation
func (s *SFU) SyncTrack(client *Client) bool {
	publishedTrackIDs := make([]string, 0)
	for _, track := range client.publishedTracks.GetTracks() {
		publishedTrackIDs = append(publishedTrackIDs, track.ID())
	}

	needRenegotiation := false

	for _, clientPeer := range s.clients.GetClients() {
		for _, track := range clientPeer.tracks.GetTracks() {
			if client.ID() != clientPeer.ID() {
				if !slices.Contains(publishedTrackIDs, track.ID()) {
					isNeedRenegotiation := client.addTrack(track)

					// request the keyframe from track publisher after added
					s.requestKeyFrameFromClient(clientPeer.ID())
					if isNeedRenegotiation {
						needRenegotiation = true
					}
				}
			}
		}
	}

	return needRenegotiation
}

func (s *SFU) Stop() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, client := range s.clients.GetClients() {
		client.PeerConnection().Close()
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
	if client, err := s.clients.GetClient(clientID); err == nil {
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
	if err := s.removeClient(client); err != nil {
		glog.Error("sfu: failed to remove client ", err)
	}
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

func (s *SFU) onTracksAvailable(tracks []ITrack) {
	for _, client := range s.clients.GetClients() {
		if !client.IsSubscribeAllTracks.Load() && client.OnTracksAvailable != nil {
			// filter out tracks from the same client
			filteredTracks := make([]ITrack, 0)
			for _, track := range tracks {
				if track.Client().ID() != client.ID() {
					filteredTracks = append(filteredTracks, track)
				}
			}

			if len(filteredTracks) > 0 {
				client.OnTracksAvailable(tracks)
			}
		}
	}
}

func (s *SFU) broadcastTracksToAutoSubscribeClients(tracks []ITrack) {
	trackReq := make([]SubscribeTrackRequest, 0)
	for _, track := range tracks {
		trackReq = append(trackReq, SubscribeTrackRequest{
			ClientID: track.Client().ID(),
			TrackID:  track.ID(),
		})
	}

	for _, client := range s.clients.GetClients() {
		if client.IsSubscribeAllTracks.Load() {
			if err := client.SubscribeTracks(trackReq); err != nil {
				glog.Error("client: failed to subscribe tracks ", err)
			}
		}
	}
}

func (s *SFU) GetClient(id string) (*Client, error) {
	return s.clients.GetClient(id)
}

func (s *SFU) removeClient(client *Client) error {
	if err := s.clients.Remove(client); err != nil {
		glog.Error("sfu: failed to remove client ", err)
		return err
	}

	s.onClientRemoved(client)

	return nil
}

func (s *SFU) SetClientsMinMaxBitrate(minBitrate, maxBitrate uint32) {
	for _, client := range s.clients.GetClients() {
		if client.OnMinMaxBitrateAdjusted != nil {
			client.OnMinMaxBitrateAdjusted(s.context, minBitrate, maxBitrate)
		}
	}
}

func (s *SFU) monitorAndAdjustBandwidth() {
	go func() {
		ctx, cancel := context.WithCancel(s.context)
		t := time.NewTicker(2 * time.Second)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				maxBitrate := uint32(highBitrate)
				minBitrate := uint32(lowBitrate)
				for _, client := range s.clients.GetClients() {
					_, bitrate := client.GetMaxBitratePerTrack()
					if maxBitrate < bitrate {
						maxBitrate = bitrate
					}

					if minBitrate > bitrate {
						minBitrate = bitrate
					}
				}

				if maxBitrate > MaxBitrateUpperCap {
					maxBitrate = MaxBitrateUpperCap
				}

				if minBitrate > MinBitrateUpperCap {
					minBitrate = MinBitrateUpperCap
				} else if minBitrate < MinBitrateLowerCap {
					minBitrate = MinBitrateLowerCap
				}

				s.SetClientsMinMaxBitrate(minBitrate, maxBitrate)
			}
		}
	}()
}

func (s *SFU) CreateDataChannel(label string, opts DataChannelOptions) error {
	dc := s.dataChannels.Get(label)
	if dc != nil {
		return ErrDataChannelExists
	}

	s.dataChannels.Add(label, opts)

	errors := []error{}
	initOpts := &webrtc.DataChannelInit{
		Ordered: &opts.Ordered,
	}

	for _, client := range s.clients.GetClients() {
		if len(opts.ClientIDs) > 0 {
			if !slices.Contains(opts.ClientIDs, client.ID()) {
				continue
			}
		}

		err := client.createDataChannel(label, initOpts)
		if err != nil {
			errors = append(errors, err)
		}
	}

	return FlattenErrors(errors)
}

func (s *SFU) setupMessageForwarder(clientID string, d *webrtc.DataChannel) {
	d.OnMessage(func(msg webrtc.DataChannelMessage) {
		// broadcast to all clients
		s.mutex.Lock()
		defer s.mutex.Unlock()

		for _, client := range s.clients.GetClients() {
			// skip the sender
			if client.id == clientID {
				continue
			}

			dc := client.dataChannels.Get(d.Label())
			if dc == nil {
				continue
			}

			if dc.ReadyState() != webrtc.DataChannelStateOpen {
				dc.OnOpen(func() {
					dc.Send(msg.Data)
				})
			} else {
				dc.Send(msg.Data)
			}
		}
	})
}

func (s *SFU) createExistingDataChannels(c *Client) {
	for _, dc := range s.dataChannels.dataChannels {
		initOpts := &webrtc.DataChannelInit{
			Ordered: &dc.isOrdered,
		}
		if len(dc.clientIDs) > 0 {
			if !slices.Contains(dc.clientIDs, c.id) {
				continue
			}
		}

		if err := c.createDataChannel(dc.label, initOpts); err != nil {
			glog.Error("datachanel: error on create existing data channels, ", err)
		}
	}
}

func (s *SFU) TotalActiveSessions() int {
	count := 0
	for _, c := range s.clients.GetClients() {
		if c.PeerConnection().ConnectionState() == webrtc.PeerConnectionStateConnected {
			count++
		}
	}

	return count
}
