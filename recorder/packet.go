package recorder

import "encoding/json"

type TrackConfig struct {
	TrackID  string
	ClientID string
	RoomID   string
	MimeType string
}

type StreamPacket struct {
	PacketType PacketType
	Payload    []byte
}

func (s *StreamPacket) MarshalJSON() ([]byte, error) {
	return json.Marshal(s)
}

func (s *StreamPacket) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, s)
}
