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

func calculateActiveSessions(clients map[string]*Client) int {
	count := 0

	for _, c := range clients {
		if c.PeerConnection().ConnectionState() == webrtc.PeerConnectionStateConnected {
			count++
		}
	}

	return count
}
