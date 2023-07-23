package sfu

import (
	"log"
	"strings"
	"time"

	"github.com/pion/webrtc/v3"
)

type Data struct {
	FromID string      `json:"from_id"`
	ToID   string      `json:"to_id"`
	SentAt time.Time   `json:"sent_at"`
	Data   interface{} `json:"data"`
}

func (s *SFU) setupDataChannelBroadcaster(peerConnection *webrtc.PeerConnection, id string) {
	// wait data channel
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		log.Println("sfu:received data channel", id, d.Label())
		if strings.HasPrefix(d.Label(), "pm-") {
			// private channel
			if _, ok := s.privateDataChannels[id]; !ok {
				s.privateDataChannels[id] = make(map[string]*webrtc.DataChannel)
			}

			IDSs := strings.Split(d.Label(), "-")
			if len(IDSs) != 2 {
				//invalid data channel label, must be in format of "fromid-toid"
				return
			}

			toID := IDSs[1]
			if _, ok := s.privateDataChannels[id][toID]; !ok {
				if client, ok := s.Clients[toID]; ok {
					dc, err := client.PeerConnection.CreateDataChannel("pm-"+id, nil)
					if err != nil {
						log.Println("sfu:error creating data channel", err)
						return
					}

					s.privateDataChannels[id][toID] = dc
				}
			}

			d.OnMessage(func(msg webrtc.DataChannelMessage) {
				// private message
				if dataChannel, ok := s.privateDataChannels[id][toID]; ok {
					if dataChannel.ReadyState() != webrtc.DataChannelStateOpen {
						dataChannel.OnOpen(func() {
							dataChannel.Send(msg.Data)
						})
					} else {
						dataChannel.Send(msg.Data)
					}
				}
			})
		} else {
			// public channel
			if _, ok := s.publicDataChannels[id]; !ok {
				s.publicDataChannels[id] = make(map[string]*webrtc.DataChannel)
			}

			if _, ok := s.publicDataChannels[id][d.Label()]; !ok {
				s.publicDataChannels[id][d.Label()] = d
			}

			d.OnMessage(func(msg webrtc.DataChannelMessage) {
				// broadcast to all clients
				for clientid, clients := range s.publicDataChannels {
					if clientid != id {
						for _, dataChannel := range clients {
							dataChannel.Send(msg.Data)
						}
					}
				}
			})
		}

	})
}
