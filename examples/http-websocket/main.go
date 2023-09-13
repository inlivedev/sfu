package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/golang/glog"
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
	TypeOffer              = "offer"
	TypeAnswer             = "answer"
	TypeCandidate          = "candidate"
	TypeError              = "error"
	TypeAllowRenegotiation = "allow_renegotiation"
	TypeTrackAdded         = "tracks_added"
	TypeTrackAvailable     = "tracks_available"
	TypeSwitchQuality      = "switch_quality"
)

func main() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := sfu.NewManager(ctx, "server-name-here", sfu.DefaultOptions())

	// generate a new room id. You can extend this example into a multiple room by use this in it's own API endpoint
	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	defaultRoom, _ := roomManager.NewRoom(roomID, roomName, sfu.RoomTypeLocal)

	fs := http.FileServer(http.Dir("./"))
	http.Handle("/", fs)

	http.Handle("/ws", websocket.Handler(func(conn *websocket.Conn) {
		messageChan := make(chan Request)
		go clientHandler(conn, messageChan, defaultRoom)
		reader(conn, messageChan)
	}))

	log.Print("Listening on http://localhost:8000 ...")

	err := http.ListenAndServe(":8000", nil)
	if err != nil {
		log.Panic(err)
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

					glog.Info("error decoding message", err)
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
	opts := sfu.DefaultClientOptions()
	opts.EnableCongestionController = true
	client, err := r.AddClient(clientID, opts)
	if err != nil {
		log.Panic(err)
		return
	}

	glog.Info("client", clientID, "added to room. Total clients", r.GetSFU().Counter)

	defer client.Stop()

	answerChan := make(chan webrtc.SessionDescription)

	client.SubscribeAllTracks()

	client.OnTracksAdded = func(tracks []sfu.ITrack) {
		tracksAdded := map[string]map[string]string{}
		for _, track := range tracks {
			tracksAdded[track.ID()] = map[string]string{"id": track.ID()}
		}
		resp := Respose{
			Status: true,
			Type:   TypeTrackAdded,
			Data:   tracksAdded,
		}

		trackAddedResp, _ := json.Marshal(resp)

		_, _ = conn.Write(trackAddedResp)
	}

	// client.OnTracksAvailable = func(tracks []sfu.ITrack) {
	// 	tracksAvailable := map[string]map[string]interface{}{}
	// 	for _, track := range tracks {

	// 		tracksAvailable[track.ID()] = map[string]interface{}{
	// 			"id":           track.ID(),
	// 			"client_id":    track.ClientID(),
	// 			"source_type":  track.SourceType().String(),
	// 			"kind":         track.Kind().String(),
	// 			"is_simulcast": track.IsSimulcast(),
	// 		}
	// 	}
	// 	resp := Respose{
	// 		Status: true,
	// 		Type:   TypeTrackAdded,
	// 		Data:   tracksAvailable,
	// 	}

	// 	trackAddedResp, _ := json.Marshal(resp)

	// 	_, _ = conn.Write(trackAddedResp)
	// }

	client.OnRenegotiation = func(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
		// SFU request a renegotiation, send the offer to client
		glog.Info("receive renegotiation offer from SFU")

		resp := Respose{
			Status: true,
			Type:   TypeOffer,
			Data:   offer,
		}

		sdpBytes, _ := json.Marshal(resp)

		_, _ = conn.Write(sdpBytes)

		// wait for answer from client
		ctxTimeout, cancelTimeout := context.WithTimeout(client.Context, 30*time.Second)

		defer cancelTimeout()

		// this will wait for answer from client in 30 seconds or timeout
		select {
		case <-ctxTimeout.Done():
			glog.Error("timeout on renegotiation")
			return webrtc.SessionDescription{}, errors.New("timeout on renegotiation")
		case answer := <-answerChan:
			glog.Info("received answer from client ", client.GetType(), client.ID)
			return answer, nil
		}
	}

	client.OnAllowedRemoteRenegotiation = func() {
		// SFU allow a remote renegotiation
		glog.Info("receive allow remote renegotiation from SFU")

		resp := Respose{
			Status: true,
			Type:   TypeAllowRenegotiation,
			Data:   "ok",
		}

		respBytes, _ := json.Marshal(resp)

		_, _ = conn.Write(respBytes)
	}

	client.OnIceCandidate = func(ctx context.Context, candidate *webrtc.ICECandidate) {
		// SFU send an ICE candidate to client
		resp := Respose{
			Status: true,
			Type:   TypeCandidate,
			Data:   candidate,
		}
		candidateBytes, _ := json.Marshal(resp)

		_, _ = conn.Write(candidateBytes)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case req := <-messageChan:
			// handle as SDP if no error
			if req.Type == TypeOffer || req.Type == TypeAnswer {
				var resp Respose

				sdp, _ := req.Data.(string)

				if req.Type == TypeOffer {
					// handle as offer SDP
					answer, err := client.Negotiate(webrtc.SessionDescription{SDP: sdp, Type: webrtc.SDPTypeOffer})
					if err != nil {
						glog.Error("error on negotiate", err)

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
					glog.Info("receive renegotiation answer from client")
					// handle as answer SDP as part of renegotiation request from SFU
					// pass the answer to onRenegotiation handler above
					answerChan <- webrtc.SessionDescription{SDP: sdp, Type: webrtc.SDPTypeAnswer}
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
			} else if req.Type == TypeTrackAdded {
				setTracks := make(map[string]sfu.TrackType, 0)
				for id, trackType := range req.Data.(map[string]interface{}) {
					if trackType.(string) == "media" {
						setTracks[id] = sfu.TrackTypeMedia
					} else {
						setTracks[id] = sfu.TrackTypeScreen
					}
				}
				client.SetTracksSourceType(setTracks)
			} else if req.Type == TypeTrackAvailable {
				subTracks, ok := req.Data.([]sfu.SubscribeTrackRequest)
				if ok {
					client.SubscribeTracks(subTracks)
				} else {
					glog.Error("error on subscribe tracks wrong data format ", req.Data)
				}

			} else if req.Type == TypeSwitchQuality {
				quality := req.Data.(string)
				switch quality {
				case "low":
					log.Println("switch to low quality")
					client.SetQuality(sfu.QualityLow)
				case "mid":
					log.Println("switch to mid quality")
					client.SetQuality(sfu.QualityMid)
				case "high":
					log.Println("switch to high quality")
					client.SetQuality(sfu.QualityHigh)
				}

			} else {
				glog.Error("unknown message type", req)
			}
		}
	}
}
