package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/inlivedev/sfu"
	"github.com/pion/webrtc/v3"
	"golang.org/x/net/websocket"
)

type Request struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type Respose struct {
	Status bool        `json:"status"`
	Type   string      `json:"type"`
	Data   interface{} `json:"data"`
}

const (
	TypeOffer     = "offer"
	TypeAnswer    = "answer"
	TypeCandidate = "candidate"
	TypeError     = "error"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := sfu.NewManager(ctx, "server-name-here", sfu.Options{})

	// generate a new room id. You can extend this example into a multiple room by use this in it's own API endpoint
	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	defaultRoom := roomManager.NewRoom(roomID, roomName, sfu.RoomTypeLocal)

	fs := http.FileServer(http.Dir("./"))
	http.Handle("/", fs)

	http.Handle("/ws", websocket.Handler(func(conn *websocket.Conn) {
		messageChan := make(chan Request)
		go clientHandler(conn, messageChan, defaultRoom)
		reader(conn, messageChan)
	}))

	log.Print("Listening on http://localhost:3000 ...")

	err := http.ListenAndServe(":3000", nil)
	if err != nil {
		log.Fatal(err)
	}

}

func reader(conn *websocket.Conn, messageChan chan Request) {
	ctx, cancel := context.WithCancel(conn.Request().Context())
	defer cancel()

MessageLoop:
	for {
		select {
		case <-ctx.Done():
			break MessageLoop
		default:
			for {
				decoder := json.NewDecoder(conn)
				var req Request
				err := decoder.Decode(&req)
				if err != nil {
					if err.Error() == "EOF" {
						continue
					}

					log.Println("error decoding message", err)
				}
				messageChan <- req
			}
		}
	}

}

func clientHandler(conn *websocket.Conn, messageChan chan Request, r *sfu.Room) {
	ctx, cancel := context.WithCancel(conn.Request().Context())
	defer cancel()

	// create new client id, you can pass a unique int value to this function
	// or just use the SFU client counter
	clientID := r.CreateClientID(r.GetSFU().Counter)

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	client, err := r.AddClient(clientID, sfu.DefaultClientOptions())
	if err != nil {
		log.Panic(err)
		return
	}

	log.Println("client", clientID, "added to room. Total clients", r.GetSFU().Counter)

	defer client.Stop()

	// offer channel is to wait for renegotation from SFU
	relayOfferChan, _ := r.GetOfferChan(clientID)

	// answer channel is pass the renegotiation answer from client to SFU
	relayAnswerChan, _ := r.GetAnswerChan(clientID)

	// candidate channel is to pass the ICE candidate SFU to client
	relayCandidateChan, _ := r.GetCandidateChan(clientID)

	for {
		select {
		case <-ctx.Done():
			return
		case req := <-messageChan:
			// handle as SDP if no error
			if req.Type == TypeOffer || req.Type == TypeAnswer {
				var resp Respose
				sdp := req.Data.(string)

				if req.Type == TypeOffer {
					// handle as offer SDP
					answer, err := client.Negotiate(webrtc.SessionDescription{SDP: sdp, Type: webrtc.SDPTypeOffer})
					if err != nil {
						log.Println("error on negotiate", err)
						resp = Respose{
							Status: false,
							Type:   TypeError,
							Data:   err.Error(),
						}
					} else {
						// send the answer to client
						resp = Respose{
							Status: true,
							Type:   TypeAnswer,
							Data:   answer,
						}
					}
					respBytes, _ := json.Marshal(resp)
					conn.Write(respBytes)
				} else {
					log.Println("receive renegotiation answer from client")
					// handle as answer SDP as part of renegotiation request from SFU
					// pass the answer to SFU through channel
					relayAnswerChan <- webrtc.SessionDescription{SDP: sdp, Type: webrtc.SDPTypeAnswer}
				}

				// don't continue execution
				continue
			} else if req.Type == TypeCandidate {
				candidate := webrtc.ICECandidateInit{
					Candidate: req.Data.(string),
				}
				err := client.AddICECandidate(candidate)
				if err != nil {
					log.Panic("error on add ice candidate", err)
				}
			} else {
				log.Println("unknown message type", req)
			}
		case offer := <-relayOfferChan:
			// SFU request a renegotiation, send the offer to client
			log.Println("receive renegotiation offer from SFU")
			resp := Respose{
				Status: true,
				Type:   TypeOffer,
				Data:   offer,
			}
			sdpBytes, _ := json.Marshal(resp)
			conn.Write(sdpBytes)
		case answer := <-relayAnswerChan:
			// SFU send an answer to respond the negotiation offer that receive from client
			resp := Respose{
				Status: true,
				Type:   TypeAnswer,
				Data:   answer,
			}
			sdpBytes, _ := json.Marshal(resp)
			conn.Write(sdpBytes)
		case candidate := <-relayCandidateChan:
			// SFU send an ICE candidate to client
			resp := Respose{
				Status: true,
				Type:   TypeCandidate,
				Data:   candidate,
			}
			candidateBytes, _ := json.Marshal(resp)
			conn.Write(candidateBytes)
		}
	}
}
