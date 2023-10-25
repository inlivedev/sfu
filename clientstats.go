package sfu

import (
	"errors"
	"sync"

	"github.com/pion/interceptor/pkg/stats"
)

var (
	ErrCLientStatsNotFound = errors.New("client stats not found")
)

type ClientStats struct {
	senderMu   sync.RWMutex
	receiverMu sync.RWMutex
	Client     *Client
	senders    map[string]stats.Stats
	receivers  map[string]stats.Stats
}

func newClientStats(c *Client) *ClientStats {
	return &ClientStats{
		senderMu:   sync.RWMutex{},
		receiverMu: sync.RWMutex{},
		Client:     c,
		senders:    make(map[string]stats.Stats),
		receivers:  make(map[string]stats.Stats),
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
