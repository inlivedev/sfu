package sfu

import (
	"context"
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
	senders          map[string]stats.Stats
	senderBitrates   map[string]uint32
	receivers        map[string]stats.Stats
	receiverBitrates map[string]uint32
}

type ClientStats struct {
	mu         sync.Mutex
	senderMu   sync.RWMutex
	receiverMu sync.RWMutex
	Client     *Client
	*TrackStats
	voiceActivity voiceActivityStats
}

func newClientStats(c *Client) *ClientStats {
	cstats := &ClientStats{
		senderMu:   sync.RWMutex{},
		receiverMu: sync.RWMutex{},
		Client:     c,
		TrackStats: &TrackStats{
			senders:          make(map[string]stats.Stats),
			receivers:        make(map[string]stats.Stats),
			senderBitrates:   make(map[string]uint32),
			receiverBitrates: make(map[string]uint32),
		},
		voiceActivity: voiceActivityStats{
			mu:     sync.Mutex{},
			active: false,
		},
	}

	go cstats.monitorBitrates(c.Context())

	return cstats
}

func (c *ClientStats) monitorBitrates(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastSenderBytesSent = make(map[string]uint64)
	var lastReceiverBytesReceived = make(map[string]uint64)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastSenderBytesSent = c.updateSenderBitrates(lastSenderBytesSent)
			lastReceiverBytesReceived = c.updateReceiverBitrates(lastReceiverBytesReceived)
		}
	}
}

func (c *ClientStats) updateSenderBitrates(lastSenderBytesSent map[string]uint64) map[string]uint64 {
	c.senderMu.Lock()
	defer c.senderMu.Unlock()

	for id, stats := range c.senders {
		lastBytesSent, ok := lastSenderBytesSent[id]
		if !ok {
			lastSenderBytesSent[id] = stats.OutboundRTPStreamStats.BytesSent
			continue
		}

		delta := stats.OutboundRTPStreamStats.BytesSent - lastBytesSent

		lastSenderBytesSent[id] = stats.OutboundRTPStreamStats.BytesSent

		c.senderBitrates[id] = uint32(delta) * 8
	}

	return lastSenderBytesSent
}

func (c *ClientStats) updateReceiverBitrates(lastReceiverBytesSent map[string]uint64) map[string]uint64 {
	c.receiverMu.Lock()
	defer c.receiverMu.Unlock()

	for id, stats := range c.receivers {
		lastBytesReceived, ok := lastReceiverBytesSent[id]
		if !ok {
			lastReceiverBytesSent[id] = stats.InboundRTPStreamStats.BytesReceived
			continue
		}

		delta := stats.InboundRTPStreamStats.BytesReceived - lastBytesReceived

		lastReceiverBytesSent[id] = stats.InboundRTPStreamStats.BytesReceived

		c.receiverBitrates[id] = uint32(delta) * 8
	}

	return lastReceiverBytesSent
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

	bitrate, ok := c.receiverBitrates[id+rid]
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
}

// UpdateVoiceActivity updates voice activity duration
// 0 timestamp means ended
func (c *ClientStats) UpdateVoiceActivity(ts uint32, clockRate uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.voiceActivity.active && ts != 0 {
		c.voiceActivity.active = true
		c.voiceActivity.start = ts
	} else if c.voiceActivity.active && ts != 0 {
		duration := (ts - c.voiceActivity.start) * 1000 / clockRate
		c.voiceActivity.start = ts
		c.voiceActivity.duration = c.voiceActivity.duration + duration
	} else if c.voiceActivity.active && ts == 0 {
		c.voiceActivity.active = false
	}
}

func (c *ClientStats) VoiceActivity() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Duration(c.voiceActivity.duration) * time.Millisecond
}
