package sfu

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
)

var (
	ErrDataChannelExists = errors.New("error: data channel already exists")
)

type SFUDataChannel struct {
	label     string
	clientIDs []string
	isOrdered bool
}

type SFUDataChannelList struct {
	dataChannels map[string]*SFUDataChannel
	mu           sync.Mutex
}

type DataChannelOptions struct {
	Ordered   bool
	ClientIDs []string // empty means all clients
}

type Data struct {
	FromID string      `json:"from_id"`
	ToID   string      `json:"to_id"`
	SentAt time.Time   `json:"sent_at"`
	Data   interface{} `json:"data"`
}

type DataChannelList struct {
	dataChannels map[string]*webrtc.DataChannel
	mu           sync.Mutex
}

func NewSFUDataChannel(label string, opts DataChannelOptions) *SFUDataChannel {
	return &SFUDataChannel{
		label:     label,
		clientIDs: opts.ClientIDs,
		isOrdered: opts.Ordered,
	}
}

func (s *SFUDataChannel) ClientIDs() []string {
	return s.clientIDs
}

func (s *SFUDataChannel) IsOrdered() bool {
	return s.isOrdered
}

func NewSFUDataChannelList() *SFUDataChannelList {
	return &SFUDataChannelList{
		dataChannels: make(map[string]*SFUDataChannel),
		mu:           sync.Mutex{},
	}
}

func (s *SFUDataChannelList) Add(label string, opts DataChannelOptions) *SFUDataChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	sfuDC := NewSFUDataChannel(label, opts)
	s.dataChannels[label] = sfuDC

	return sfuDC
}

func (s *SFUDataChannelList) Remove(dc *SFUDataChannel) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.dataChannels, dc.label)
}

func (s *SFUDataChannelList) Get(label string) *SFUDataChannel {
	s.mu.Lock()
	defer s.mu.Unlock()

	dc, ok := s.dataChannels[label]
	if !ok {
		return nil
	}

	return dc
}

func DefaultDataChannelOptions() DataChannelOptions {
	return DataChannelOptions{
		Ordered:   true,
		ClientIDs: []string{},
	}
}

func NewDataChannelList(ctx context.Context) *DataChannelList {
	list := &DataChannelList{
		dataChannels: make(map[string]*webrtc.DataChannel),
		mu:           sync.Mutex{},
	}

	go func() {
		localCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		<-localCtx.Done()
		list.Clear()
	}()

	return list
}

func (d *DataChannelList) Add(dc *webrtc.DataChannel) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.dataChannels[dc.Label()] = dc
}

func (d *DataChannelList) Remove(dc *webrtc.DataChannel) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.dataChannels, dc.Label())
}

func (d *DataChannelList) Get(label string) *webrtc.DataChannel {
	d.mu.Lock()
	defer d.mu.Unlock()

	dc, ok := d.dataChannels[label]
	if !ok {
		return nil
	}

	return dc
}

func (d *DataChannelList) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for label, dc := range d.dataChannels {
		dc.Close()
		delete(d.dataChannels, label)
	}
}
