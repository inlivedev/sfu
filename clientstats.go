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

type voiceActivityStats struct {
	mu       sync.Mutex
	active   bool
	start    uint32
	duration uint32
}

type ClientStats struct {
	mu            sync.Mutex
	senderMu      sync.RWMutex
	receiverMu    sync.RWMutex
	Client        *Client
	senders       map[string]stats.Stats
	receivers     map[string]stats.Stats
	voiceActivity voiceActivityStats
}

func newClientStats(c *Client) *ClientStats {
	return &ClientStats{
		senderMu:   sync.RWMutex{},
		receiverMu: sync.RWMutex{},
		Client:     c,
		senders:    make(map[string]stats.Stats),
		receivers:  make(map[string]stats.Stats),
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

func (c *ClientStats) GetReceiver(id string) (stats.Stats, error) {
	c.receiverMu.RLock()
	defer c.receiverMu.RUnlock()

	receiver, ok := c.receivers[id]
	if !ok {
		return stats.Stats{}, ErrCLientStatsNotFound
	}

	return receiver, nil
}

func (c *ClientStats) SetReceiver(id string, stats stats.Stats) {
	c.receiverMu.Lock()
	defer c.receiverMu.Unlock()

	c.receivers[id] = stats
}

// UpdateVoiceActivity updates voice activity duration
// 0 timestamp means ended
func (c *ClientStats) UpdateVoiceActivity(ts uint32) {
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
