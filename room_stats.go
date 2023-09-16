package sfu

import (
	"time"

	"github.com/pion/webrtc/v3"
)

type StatTracks struct {
	Audio int `json:"audio"`
	Video int `json:"video"`
}

type RoomStats struct {
	ActiveSessions int        `json:"active_sessions"`
	Clients        int        `json:"clients"`
	PacketLost     int64      `json:"packet_lost"`
	ByteSent       uint64     `json:"bytes_sent"`
	BytesReceived  uint64     `json:"bytes_received"`
	Tracks         StatTracks `json:"tracks"`
	Timestamp      time.Time  `json:"timestamp"`
}

func generateCurrentStats(r *Room) RoomStats {
	var (
		bytesReceived uint64
		bytesSent     uint64
		packet_lost   int64
	)
	clients := r.GetSFU().GetClients()
	for _, c := range clients {
		stats := c.Stats()
		for _, stat := range stats.Receiver {
			bytesReceived += stat.InboundRTPStreamStats.BytesReceived
			packet_lost += stat.InboundRTPStreamStats.PacketsLost
		}

		for _, stat := range stats.Sender {
			bytesSent += stat.OutboundRTPStreamStats.BytesSent
		}
	}
	return RoomStats{
		ActiveSessions: calculateActiveSessions(r.GetSFU().GetClients()),
		Clients:        len(r.GetSFU().GetClients()),
		PacketLost:     packet_lost,
		BytesReceived:  bytesReceived,
		ByteSent:       bytesSent,
		Timestamp:      time.Now(),
	}
}

func calculateActiveSessions(clients map[string]*Client) int {
	count := 0

	for _, c := range clients {
		if c.PeerConnection().ConnectionState() == webrtc.PeerConnectionStateConnected {
			count++
		}
	}

	return count
}
