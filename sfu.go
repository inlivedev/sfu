package sfu

import (
	"context"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"golang.org/x/exp/slices"
)

// BitrateConfigs is the configuration for the bitrate that will be used for adaptive bitrates controller
// The paramenter is in bps (bit per second) for non pixels parameters.
// For pixels parameters, it is total pixels (width * height) of the video.
// High, Mid, and Low are the references for bitrate controller to decide the max bitrate to send to the client.
type BitrateConfigs struct {
	AudioRed         uint32 `json:"audio_red,omitempty" yaml:"audio_red,omitempty" mapstructure:"audio_red,omitempty"`
	Audio            uint32 `json:"audio,omitempty" yaml:"audio,omitempty" mapstructure:"audio,omitempty"`
	Video            uint32 `json:"video,omitempty" yaml:"video,omitempty" mapstructure:"video,omitempty"`
	VideoHigh        uint32 `json:"video_high,omitempty" yaml:"video_high,omitempty" mapstructure:"video_high,omitempty"`
	VideoHighPixels  uint32 `json:"video_high_pixels,omitempty" yaml:"video_high_pixels,omitempty" mapstructure:"video_high_pixels,omitempty"`
	VideoMid         uint32 `json:"video_mid,omitempty" yaml:"video_mid,omitempty" mapstructure:"video_mid,omitempty"`
	VideoMidPixels   uint32 `json:"video_mid_pixels,omitempty" yaml:"video_mid_pixels,omitempty" mapstructure:"video_mid_pixels,omitempty"`
	VideoLow         uint32 `json:"video_low,omitempty" yaml:"video_low,omitempty" mapstructure:"video_low,omitempty"`
	VideoLowPixels   uint32 `json:"video_low_pixels,omitempty" yaml:"video_low_pixels,omitempty" mapstructure:"video_low_pixels,omitempty"`
	InitialBandwidth uint32 `json:"initial_bandwidth,omitempty" yaml:"initial_bandwidth,omitempty" mapstructure:"initial_bandwidth,omitempty"`
}

func DefaultBitrates() BitrateConfigs {
	return BitrateConfigs{
		AudioRed:         65_000,
		Audio:            48_000,
		Video:            1_200_000,
		VideoHigh:        1_200_000,
		VideoHighPixels:  720 * 360,
		VideoMid:         500_000,
		VideoMidPixels:   360 * 180,
		VideoLow:         150_000,
		VideoLowPixels:   180 * 90,
		InitialBandwidth: 1_000_000,
	}
}

type SFUClients struct {
	clients map[string]*Client
	mu      sync.Mutex
}

func (s *SFUClients) GetClients() map[string]*Client {
	s.mu.Lock()
	defer s.mu.Unlock()

	clients := make(map[string]*Client)
	for k, v := range s.clients {
		clients[k] = v
	}

	return clients
}

func (s *SFUClients) GetClient(id string) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if client, ok := s.clients[id]; ok {
		return client, nil
	}

	return nil, ErrClientNotFound
}

func (s *SFUClients) Length() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.clients)
}

func (s *SFUClients) Add(client *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[client.ID()]; ok {
		return ErrClientExists
	}

	s.clients[client.ID()] = client

	return nil
}

func (s *SFUClients) Remove(client *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[client.ID()]; !ok {
		return ErrClientNotFound
	}

	delete(s.clients, client.ID())

	return nil
}

type SFU struct {
	bitrateConfigs            BitrateConfigs
	clients                   *SFUClients
	context                   context.Context
	cancel                    context.CancelFunc
	codecs                    []string
	dataChannels              *SFUDataChannelList
	iceServers                []webrtc.ICEServer
	nat1To1IPsCandidateType   webrtc.ICECandidateType
	mu                        sync.Mutex
	mux                       *UDPMux
	onStop                    func()
	pliInterval               time.Duration
	qualityRef                QualityPreset
	portStart                 uint16
	portEnd                   uint16
	publicIP                  string
	onTrackAvailableCallbacks []func(tracks []ITrack)
	onClientRemovedCallbacks  []func(*Client)
	onClientAddedCallbacks    []func(*Client)
	relayTracks               map[string]ITrack
}

type PublishedTrack struct {
	ClientID string
	Track    webrtc.TrackLocal
}

type sfuOptions struct {
	IceServers              []webrtc.ICEServer
	Mux                     *UDPMux
	PortStart               uint16
	PortEnd                 uint16
	Bitrates                BitrateConfigs
	QualityPreset           QualityPreset
	Codecs                  []string
	PLIInterval             time.Duration
	PublicIP                string
	NAT1To1IPsCandidateType webrtc.ICECandidateType
}

// @Param muxPort: port for udp mux
func New(ctx context.Context, opts sfuOptions) *SFU {
	localCtx, cancel := context.WithCancel(ctx)

	sfu := &SFU{
		clients:                   &SFUClients{clients: make(map[string]*Client), mu: sync.Mutex{}},
		context:                   localCtx,
		cancel:                    cancel,
		codecs:                    opts.Codecs,
		dataChannels:              NewSFUDataChannelList(),
		mu:                        sync.Mutex{},
		iceServers:                opts.IceServers,
		mux:                       opts.Mux,
		bitrateConfigs:            opts.Bitrates,
		pliInterval:               opts.PLIInterval,
		qualityRef:                opts.QualityPreset,
		publicIP:                  opts.PublicIP,
		relayTracks:               make(map[string]ITrack),
		portStart:                 opts.PortStart,
		portEnd:                   opts.PortEnd,
		onTrackAvailableCallbacks: make([]func(tracks []ITrack), 0),
		onClientRemovedCallbacks:  make([]func(*Client), 0),
		onClientAddedCallbacks:    make([]func(*Client), 0),
		nat1To1IPsCandidateType:   opts.NAT1To1IPsCandidateType,
	}

	return sfu
}

func (s *SFU) addClient(client *Client) {
	if err := s.clients.Add(client); err != nil {
		glog.Error("sfu: failed to add client ", err)
		return
	}

	s.onClientAdded(client)
}

func (s *SFU) createClient(id string, name string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {

	client := NewClient(s, id, name, peerConnectionConfig, opts)

	// Get the LocalDescription and take it to base64 so we can paste in browser
	return client
}

func (s *SFU) NewClient(id, name string, opts ClientOptions) *Client {
	peerConnectionConfig := webrtc.Configuration{}

	if len(s.iceServers) > 0 {
		peerConnectionConfig.ICEServers = s.iceServers
	}

	client := s.createClient(id, name, peerConnectionConfig, opts)

	client.onTrack = func(track ITrack) {
		if err := client.pendingPublishedTracks.Add(track); err == ErrTrackExists {
			glog.Error("sfu: client ", id, " track already added ", track.ID())
			// not an error could be because a simulcast track already added
			return
		}

		// don't publish track when not all the tracks are received
		// TODO:
		// 1. need to handle simulcast track because  it will be counted as single track
		initialReceiverCount := client.initialReceiverCount.Load()
		if client.Type() == ClientTypePeer && int(initialReceiverCount) > client.pendingPublishedTracks.Length() {
			glog.Info("sfu: client ", id, " pending published tracks: ", client.pendingPublishedTracks.Length(), " initial tracks count: ", initialReceiverCount)
			return
		}

		glog.Info("sfu: client ", id, " publish tracks, initial tracks count: ", initialReceiverCount, " pending published tracks: ", client.pendingPublishedTracks.Length())

		addedTracks := client.pendingPublishedTracks.GetTracks()

		if client.onTracksAdded != nil {
			client.onTracksAdded(addedTracks)
		}

		// broadcast to client with auto subscribe tracks
		s.broadcastTracksToAutoSubscribeClients(client.ID(), addedTracks)
	}

	client.peerConnection.PC().OnSignalingStateChange(func(state webrtc.SignalingState) {
		if state == webrtc.SignalingStateStable && client.pendingRemoteRenegotiation.Load() {
			client.pendingRemoteRenegotiation.Store(false)
			client.allowRemoteRenegotiation()
		}
	})

	s.addClient(client)

	return client
}

func (s *SFU) AvailableTracks() []ITrack {
	tracks := make([]ITrack, 0)

	for _, client := range s.clients.GetClients() {
		tracks = append(tracks, client.publishedTracks.GetTracks()...)
	}

	return tracks
}

// Syncs track from connected client to other clients
func (s *SFU) syncTrack(client *Client) {
	publishedTrackIDs := make([]string, 0)
	for _, track := range client.publishedTracks.GetTracks() {
		publishedTrackIDs = append(publishedTrackIDs, track.ID())
	}

	subscribes := make([]SubscribeTrackRequest, 0)

	for _, clientPeer := range s.clients.GetClients() {
		for _, track := range clientPeer.tracks.GetTracks() {
			if client.ID() != clientPeer.ID() {
				if !slices.Contains(publishedTrackIDs, track.ID()) {
					subscribes = append(subscribes, SubscribeTrackRequest{
						ClientID: clientPeer.ID(),
						TrackID:  track.ID(),
					})
				}
			}
		}
	}

	if len(subscribes) > 0 {
		err := client.SubscribeTracks(subscribes)
		if err != nil {
			glog.Error("client: failed to subscribe tracks ", err)
		}
	}
}

func (s *SFU) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, client := range s.clients.GetClients() {
		client.PeerConnection().Close()
	}

	if s.onStop != nil {
		s.onStop()
	}

	s.cancel()

}

func (s *SFU) OnStopped(callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onStop = callback
}

func (s *SFU) OnClientAdded(callback func(*Client)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onClientAddedCallbacks = append(s.onClientAddedCallbacks, callback)
}

func (s *SFU) OnClientRemoved(callback func(*Client)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onClientRemovedCallbacks = append(s.onClientRemovedCallbacks, callback)
}

func (s *SFU) onAfterClientStopped(client *Client) {
	if err := s.removeClient(client); err != nil {
		glog.Error("sfu: failed to remove client ", err)
	}
}

func (s *SFU) onClientAdded(client *Client) {
	for _, callback := range s.onClientAddedCallbacks {
		callback(client)
	}
}

func (s *SFU) onClientRemoved(client *Client) {
	for _, callback := range s.onClientRemovedCallbacks {
		callback(client)
	}
}

func (s *SFU) onTracksAvailable(clientId string, tracks []ITrack) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, client := range s.clients.GetClients() {
		if client.ID() != clientId {
			client.onTracksAvailable(tracks)
			glog.Info("sfu: client ", client.ID(), " tracks available ", len(tracks))
		}
	}

	for _, callback := range s.onTrackAvailableCallbacks {
		if callback != nil {
			callback(tracks)
		}
	}
}

func (s *SFU) broadcastTracksToAutoSubscribeClients(ownerID string, tracks []ITrack) {
	trackReq := make([]SubscribeTrackRequest, 0)
	for _, track := range tracks {
		trackReq = append(trackReq, SubscribeTrackRequest{
			ClientID: track.ClientID(),
			TrackID:  track.ID(),
		})
	}

	for _, client := range s.clients.GetClients() {
		if ownerID != client.ID() {
			if err := client.SubscribeTracks(trackReq); err != nil {
				glog.Error("client: failed to subscribe tracks ", err)
			}
		}
	}
}

func (s *SFU) GetClient(id string) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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

func (s *SFU) CreateDataChannel(label string, opts DataChannelOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
		s.mu.Lock()
		defer s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, c := range s.clients.GetClients() {
		if c.PeerConnection().PC().ConnectionState() == webrtc.PeerConnectionStateConnected {
			count++
		}
	}

	return count
}

func (s *SFU) QualityLevelToBitrate(level QualityLevel) uint32 {
	switch level {
	case QualityAudioRed:
		return s.bitrateConfigs.AudioRed
	case QualityAudio:
		return s.bitrateConfigs.Audio
	case QualityLow:
		return s.bitrateConfigs.VideoLow
	case QualityMid:
		return s.bitrateConfigs.VideoMid
	case QualityHigh:
		return s.bitrateConfigs.VideoHigh
	default:
		return 0
	}
}

func (s *SFU) PLIInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pliInterval
}

func (s *SFU) QualityPreset() QualityPreset {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.qualityRef
}

func (s *SFU) OnTracksAvailable(callback func(tracks []ITrack)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onTrackAvailableCallbacks = append(s.onTrackAvailableCallbacks, callback)
}

func (s *SFU) AddRelayTrack(ctx context.Context, id, streamid, rid, clientid string, kind webrtc.RTPCodecType, ssrc webrtc.SSRC, mimeType string, rtpChan chan *rtp.Packet) error {
	var track ITrack

	relayTrack := NewTrackRelay(id, streamid, rid, kind, ssrc, mimeType, rtpChan)

	onPLI := func() {}

	if rid == "" {
		// not simulcast
		track = newTrack(ctx, clientid, relayTrack, 0, 0, s.pliInterval, onPLI, nil, nil)
		s.mu.Lock()
		s.relayTracks[relayTrack.ID()] = track
		s.mu.Unlock()
	} else {
		// simulcast
		var simulcast *SimulcastTrack
		var ok bool

		s.mu.Lock()
		track, ok := s.relayTracks[relayTrack.ID()]
		if !ok {
			// if track not found, add it
			track = newSimulcastTrack(ctx, clientid, relayTrack, 0, 0, s.pliInterval, onPLI, nil, nil)
			s.relayTracks[relayTrack.ID()] = track

		} else if simulcast, ok = track.(*SimulcastTrack); ok {
			simulcast.AddRemoteTrack(simulcast.context, relayTrack, 0, 0, nil, nil)
		}
		s.mu.Unlock()
	}

	s.broadcastTracksToAutoSubscribeClients(clientid, []ITrack{track})

	// notify the local clients that a relay track is available
	s.onTracksAvailable(clientid, []ITrack{track})

	return nil
}
