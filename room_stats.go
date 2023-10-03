package sfu

import (
	"time"
)

type StatTracks struct {
	Audio int `json:"audio"`
	Video int `json:"video"`
}

type TrackSentStats struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	PacketsLost int64  `json:"packets_lost"`
	PacketSent  uint64 `json:"packets_sent"`
	ByteSent    uint64 `json:"bytes_sent"`
}

type TrackReceiveStats struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Codec          string `json:"codec"`
	PacketsLost    int64  `json:"packets_lost"`
	PacketReceived uint64 `json:"packets_received"`
	ByteReceived   int64  `json:"bytes_received"`
}

type ClientTrackStats struct {
	Sents    []TrackSentStats    `json:"sent_track_stats"`
	Receives []TrackReceiveStats `json:"receive_track_stats"`
}

type RoomStats struct {
	ActiveSessions     int                          `json:"active_sessions"`
	ClientsCount       int                          `json:"clients_count"`
	PacketSentLost     int64                        `json:"packet_sent_lost"`
	PacketReceivedLost int64                        `json:"packet_received_lost"`
	PacketReceived     uint64                       `json:"packet_received"`
	PacketSent         uint64                       `json:"packet_sent"`
	ByteSent           uint64                       `json:"bytes_sent"`
	BytesReceived      uint64                       `json:"bytes_received"`
	Tracks             StatTracks                   `json:"tracks"`
	Timestamp          time.Time                    `json:"timestamp"`
	ClientStats        map[string]*ClientTrackStats `json:"client_stats"`
}
