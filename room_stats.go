package sfu

import (
	"time"
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
