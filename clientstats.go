package sfu

import (
	"errors"
	"sync"
	"time"

	"github.com/pion/interceptor/pkg/stats"
)

var (
	ErrCLientStatsNotFound = errors.New("client stats not found")
)

type staticVoiceActivityStats struct {
	start    uint32
	duration uint32
}

type voiceActivityStats struct {
	mu     sync.Mutex
	active bool
	staticVoiceActivityStats
}

type TrackStats struct {
	senders           map[string]stats.Stats
	senderBitrates    map[string]uint32
	bytesSent         map[string]uint64
	receivers         map[string]stats.Stats
	receiversBitrates map[string]uint32
	bytesReceived     map[string]uint64
}

type ClientStats struct {
	mu         sync.Mutex
	senderMu   sync.RWMutex
	receiverMu sync.RWMutex
	Client     *Client
	TrackStats
	voiceActivity voiceActivityStats
}

func newClientStats(c *Client) *ClientStats {
	return &ClientStats{
		senderMu:   sync.RWMutex{},
		receiverMu: sync.RWMutex{},
		Client:     c,
		TrackStats: TrackStats{
			senders:           make(map[string]stats.Stats),
			receivers:         make(map[string]stats.Stats),
			senderBitrates:    make(map[string]uint32),
			receiversBitrates: make(map[string]uint32),
			bytesSent:         make(map[string]uint64),
			bytesReceived:     make(map[string]uint64),
		},
		voiceActivity: voiceActivityStats{
			mu:     sync.Mutex{},
			active: false,
		},
	}
}

func (c *ClientStats) removeSenderStats(trackId string) {
	c.senderMu.Lock()
	defer c.senderMu.Unlock()

	delete(c.senders, trackId)
}

func (c *ClientStats) removeReceiverStats(trackId string) {
	c.receiverMu.Lock()
	defer c.receiverMu.Unlock()

	delete(c.receivers, trackId)
}

func (c *ClientStats) Senders() map[string]stats.Stats {
	c.senderMu.RLock()
	defer c.senderMu.RUnlock()

	stats := make(map[string]stats.Stats)
	for k, v := range c.senders {
		stats[k] = v
	}

	return stats
}

func (c *ClientStats) GetSender(id string) (stats.Stats, error) {
	c.senderMu.RLock()
	defer c.senderMu.RUnlock()

	sender, ok := c.senders[id]
	if !ok {
		return stats.Stats{}, ErrCLientStatsNotFound
	}

	return sender, nil
}

func (c *ClientStats) GetSenderBitrate(id string) (uint32, error) {
	c.senderMu.RLock()
	defer c.senderMu.RUnlock()

	bitrate, ok := c.senderBitrates[id]
	if !ok {
		return 0, ErrCLientStatsNotFound
	}

	return bitrate, nil
}

func (c *ClientStats) SetSender(id string, stats stats.Stats) {
	c.senderMu.Lock()
	defer c.senderMu.Unlock()

	c.senders[id] = stats
	if _, ok := c.bytesSent[id]; !ok {
		c.bytesSent[id] = stats.OutboundRTPStreamStats.BytesSent
		c.senderBitrates[id] = uint32(c.bytesSent[id]) * 8
	} else {
		delta := stats.OutboundRTPStreamStats.BytesSent - c.bytesSent[id]
		c.bytesSent[id] = stats.OutboundRTPStreamStats.BytesSent
		c.senderBitrates[id] = uint32(delta) * 8
	}
}

func (c *ClientStats) Receivers() map[string]stats.Stats {
	c.receiverMu.RLock()
	defer c.receiverMu.RUnlock()

	stats := make(map[string]stats.Stats)
	for k, v := range c.receivers {
		stats[k] = v
	}

	return stats
}

func (c *ClientStats) GetReceiverBitrate(id, rid string) (uint32, error) {
	c.receiverMu.RLock()
	defer c.receiverMu.RUnlock()

	bitrate, ok := c.receiversBitrates[id+rid]
	if !ok {
		return 0, ErrCLientStatsNotFound
	}

	return bitrate, nil
}

func (c *ClientStats) GetReceiver(id, rid string) (stats.Stats, error) {
	c.receiverMu.RLock()
	defer c.receiverMu.RUnlock()

	receiver, ok := c.receivers[id+rid]
	if !ok {
		return stats.Stats{}, ErrCLientStatsNotFound
	}

	return receiver, nil
}

func (c *ClientStats) SetReceiver(id, rid string, stats stats.Stats) {
	c.receiverMu.Lock()
	defer c.receiverMu.Unlock()

	idrid := id + rid

	c.receivers[idrid] = stats
	if _, ok := c.bytesReceived[idrid]; !ok {
		c.bytesReceived[idrid] = stats.InboundRTPStreamStats.BytesReceived
		c.receiversBitrates[idrid] = uint32(c.bytesReceived[idrid]) * 8
	} else {
		delta := stats.InboundRTPStreamStats.BytesReceived - c.bytesReceived[id]
		c.receiversBitrates[idrid] = uint32(delta) * 8
		c.bytesReceived[idrid] = stats.InboundRTPStreamStats.BytesReceived
	}
}

// UpdateVoiceActivity updates voice activity duration
// 0 timestamp means ended
func (c *ClientStats) UpdateVoiceActivity(ts uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.voiceActivity.active && ts != 0 {
		c.voiceActivity.active = true
		c.voiceActivity.start = ts
		return
	} else if c.voiceActivity.active && ts != 0 {
		c.voiceActivity.duration = ts - c.voiceActivity.start
	} else if c.voiceActivity.active && ts == 0 {
		c.voiceActivity.active = false
		c.voiceActivity.duration = ts - c.voiceActivity.start
	}
}

func (c *ClientStats) VoiceActivity() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Duration(c.voiceActivity.duration) * time.Millisecond
}
