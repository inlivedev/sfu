package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
	"github.com/samespace/sfu"
	"github.com/samespace/sfu/pkg/fakeclient"
	"github.com/samespace/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/samespace/sfu/pkg/networkmonitor"
	"github.com/samespace/sfu/recorder"
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

type VAD struct {
	SSRC     uint32                                `json:"ssrc"`
	TrackID  string                                `json:"track_id"`
	StreamID string                                `json:"stream_id"`
	Packets  []voiceactivedetector.VoicePacketData `json:"packets"`
}

type AvailableTrack struct {
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
	TrackID    string `json:"track_id"`
	StreamID   string `json:"stream_id"`
	Source     string `json:"source"`
}

const (
	TypeOffer                = "offer"
	TypeAnswer               = "answer"
	TypeCandidate            = "candidate"
	TypeNetworkCondition     = "network_condition"
	TypeError                = "error"
	TypeAllowRenegotiation   = "allow_renegotiation"
	TypeIsAllowRenegotiation = "is_allow_renegotiation"
	TypeTrackAdded           = "tracks_added"
	TypeTracksAvailable      = "tracks_available"
	TypeSubscribeTracks      = "subscribe_tracks"
	TypeSwitchQuality        = "switch_quality"
	TypeUpdateBandwidth      = "update_bandwidth"
	TypeSetBandwidthLimit    = "set_bandwidth_limit"
	TypeBitrateAdjusted      = "bitrate_adjusted"
	TypeTrackStats           = "track_stats"
	TypeVoiceDetected        = "voice_detected"
)

var logger logging.LeveledLogger

func main() {

	var err error

	/* conn, err := recorder.NewQuicClient(context.Background(), recorder.ClientConfig{
		ClientId: "test-client",
		FileName: "test-room",
	}, &recorder.QuicConfig{
		Host:     "127.0.0.1",
		Port:     9000,
		CertFile: "server.cert",
		KeyFile:  "server.key",
	})

	fmt.Println(err)

	if err != nil {
		panic(err)
	}

	stream, err := conn.OpenUniStream()

	fmt.Println(err)
	if err != nil {
		panic(err)
	}

	rec, err := recorder.NewQuickTrackRecorder(&recorder.TrackConfig{
		TrackID:  "test-track",
		ClientID: "test-client",
		RoomID:   "test-room",
		MimeType: "audio/opus",
	}, stream)

	if err != nil {
		panic(err)
	}

	fmt.Println(rec)
	*/

	flag.Set("logtostderr", "true")
	// flag.Set("stderrthreshold", "DEBUG")
	// flag.Set("PIONS_LOG_INFO", "sfu,vad")

	flag.Set("PIONS_LOG_ERROR", "sfu,vad")
	flag.Set("PIONS_LOG_WARN", "sfu,vad")
	// flag.Set("PIONS_LOG_DEBUG", "sfu,vad")
	// flag.Set("PIONS_LOG_TRACE", "sfu,vad")

	flag.Parse()

	logger = logging.NewDefaultLoggerFactory().NewLogger("sfu")

	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	sfuOpts := sfu.DefaultOptions()

	sfuOpts.EnableBandwidthEstimator = true

	fakeClientCount := 0

	_, turnEnabled := os.LookupEnv("TURN_ENABLED")
	if turnEnabled || fakeClientCount > 0 {
		sfu.StartStunServer(ctx, "127.0.0.1")
		sfuOpts.IceServers = append(sfuOpts.IceServers, webrtc.ICEServer{
			URLs: []string{"stun:127.0.0.1:3478"},
		})
	}

	// create room manager first before create new room
	roomManager := sfu.NewManager(ctx, "server-name-here", sfuOpts)

	// generate a new room id. You can extend this example into a multiple room by use this in it's own API endpoint
	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomsOpts := sfu.DefaultRoomOptions()
	roomsOpts.QuicConfig = []*recorder.QuicConfig{
		{
			Host:     "127.0.0.1",
			Port:     9000,
			CertFile: "server.cert",
			KeyFile:  "server.key",
		},
	}
	roomsOpts.Bitrates.InitialBandwidth = 1_000_000
	// roomsOpts.PLIInterval = 3 * time.Second
	defaultRoom, _ := roomManager.NewRoom(roomID, roomName, sfu.RoomTypeLocal, roomsOpts)
	// turnServer := sfu.StartTurnServer(ctx, localIp.String())
	// defer turnServer.Close()

	iceServers := []webrtc.ICEServer{
		{
			URLs: []string{"stun:127.0.0.1:3478"},
		},
	}

	for i := 0; i < fakeClientCount; i++ {
		// create a fake client
		fc := fakeclient.Create(ctx, roomManager.Log(), defaultRoom, iceServers, fmt.Sprintf("fake-client-%d", i), true)

		fc.Client.OnTracksAdded(func(addedTracks []sfu.ITrack) {
			setTracks := make(map[string]sfu.TrackType, 0)
			for _, track := range addedTracks {
				setTracks[track.ID()] = sfu.TrackTypeMedia
			}
			fc.Client.SetTracksSourceType(setTracks)
		})
	}

	fs := http.FileServer(http.Dir("./"))
	http.Handle("/", fs)

	http.Handle("/ws", websocket.Handler(func(conn *websocket.Conn) {
		messageChan := make(chan Request)
		isDebug := false
		if conn.Request().URL.Query().Get("debug") != "" {
			isDebug = true
		}
		go clientHandler(isDebug, conn, messageChan, defaultRoom)
		reader(conn, messageChan)
	}))

	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		statsHandler(w, r, defaultRoom)
	})

	logger.Info("Listening on http://localhost:8000 ...")

	err = http.ListenAndServe(":8000", nil)
	if err != nil {
		log.Panic(err)
	}
}

func statsHandler(w http.ResponseWriter, _ *http.Request, room *sfu.Room) {
	stats := room.Stats()

	statsJSON, _ := json.Marshal(stats)

	w.Header().Set("Content-Type", "application/json")

	_, _ = w.Write([]byte(statsJSON))
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

					logger.Infof("error decoding message", err)
				}
				messageChan <- req
			}
		}
	}
}

func clientHandler(isDebug bool, conn *websocket.Conn, messageChan chan Request, r *sfu.Room) {
	ctx, cancel := context.WithCancel(conn.Request().Context())
	defer cancel()

	// create new client id, you can pass a unique int value to this function
	// or just use the SFU client counter
	clientID := r.CreateClientID()

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	opts := sfu.DefaultClientOptions()
	opts.EnableVoiceDetection = true
	opts.ReorderPackets = false
	client, err := r.AddClient(clientID, clientID, opts)
	if err != nil {
		log.Panic(err)
		return
	}

	if isDebug {
		client.EnableDebug()
	}

	defer r.StopClient(client.ID())

	_, _ = conn.Write([]byte("{\"type\":\"clientid\",\"data\":\"" + clientID + "\"}"))

	answerChan := make(chan webrtc.SessionDescription)

	// client.SubscribeAllTracks()

	client.OnTracksAdded(func(tracks []sfu.ITrack) {
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
	})

	client.OnTracksAvailable(func(tracks []sfu.ITrack) {
		if client.IsDebugEnabled() {
			logger.Infof("tracks available", tracks)
		}
		tracksAvailable := map[string]map[string]interface{}{}
		for _, track := range tracks {

			tracksAvailable[track.ID()] = map[string]interface{}{
				"id":           track.ID(),
				"client_id":    track.ClientID(),
				"source_type":  track.SourceType().String(),
				"kind":         track.Kind().String(),
				"is_simulcast": track.IsSimulcast(),
			}
		}
		resp := Respose{
			Status: true,
			Type:   TypeTracksAvailable,
			Data:   tracksAvailable,
		}

		trackAddedResp, _ := json.Marshal(resp)

		_, _ = conn.Write(trackAddedResp)
	})

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
		// SFU request a renegotiation, send the offer to client
		logger.Infof("receive renegotiation offer from SFU")

		resp := Respose{
			Status: true,
			Type:   TypeOffer,
			Data:   offer,
		}

		sdpBytes, _ := json.Marshal(resp)

		_, _ = conn.Write(sdpBytes)

		// wait for answer from client
		ctxTimeout, cancelTimeout := context.WithTimeout(client.Context(), 30*time.Second)

		defer cancelTimeout()

		// this will wait for answer from client in 30 seconds or timeout
		select {
		case <-ctxTimeout.Done():
			logger.Errorf("timeout on renegotiation")
			return webrtc.SessionDescription{}, errors.New("timeout on renegotiation")
		case answer := <-answerChan:
			logger.Infof("received answer from client ", client.Type(), client.ID())
			return answer, nil
		}
	})

	client.OnAllowedRemoteRenegotiation(func() {
		// SFU allow a remote renegotiation
		logger.Infof("receive allow remote renegotiation from SFU")

		resp := Respose{
			Status: true,
			Type:   TypeAllowRenegotiation,
			Data:   "ok",
		}

		respBytes, _ := json.Marshal(resp)

		_, _ = conn.Write(respBytes)
	})

	type Bitrates struct {
		Min         uint32 `json:"min"`
		Max         uint32 `json:"max"`
		TotalClient uint32 `json:"total_client"`
	}

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		// SFU send an ICE candidate to client
		resp := Respose{
			Status: true,
			Type:   TypeCandidate,
			Data:   candidate,
		}
		candidateBytes, _ := json.Marshal(resp)

		_, _ = conn.Write(candidateBytes)
	})

	client.OnNetworkConditionChanged(func(condition networkmonitor.NetworkConditionType) {
		resp := Respose{
			Status: true,
			Type:   TypeNetworkCondition,
			Data:   condition,
		}
		respBytes, _ := json.Marshal(resp)

		_, _ = conn.Write(respBytes)
	})

	done := make(chan bool)

	player, err := client.GetPlayer(done)
	if err != nil {
		log.Panic(err)
		return
	}

	go func() {
		time.Sleep(5 * time.Second)
		fmt.Println(player.PlayFile("audio.opus"))

		<-done
		fmt.Println("done")
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := client.Stats()

			resp := Respose{
				Status: true,
				Type:   TypeTrackStats,
				Data:   stats,
			}

			respBytes, _ := json.Marshal(resp)
			_, _ = conn.Write(respBytes)

		case req := <-messageChan:
			// handle as SDP if no error
			if req.Type == TypeOffer || req.Type == TypeAnswer {
				var resp Respose

				sdp, _ := req.Data.(string)

				if req.Type == TypeOffer {
					// handle as offer SDP
					answer, err := client.Negotiate(webrtc.SessionDescription{SDP: sdp, Type: webrtc.SDPTypeOffer})
					if err != nil {
						logger.Errorf("error on negotiate", err)

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
					logger.Infof("receive renegotiation answer from client")
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
			} else if req.Type == TypeSubscribeTracks {
				subTracks := make([]sfu.SubscribeTrackRequest, 0)
				tracks, ok := req.Data.([]interface{})
				if ok {
					for _, track := range tracks {
						trackData, ok := track.(map[string]interface{})
						if ok {
							subTrack := sfu.SubscribeTrackRequest{
								ClientID: trackData["client_id"].(string),
								TrackID:  trackData["track_id"].(string),
							}

							subTracks = append(subTracks, subTrack)
						}
					}

					if err := client.SubscribeTracks(subTracks); err != nil {
						logger.Errorf("error on subscribe tracks", err)
					}
				} else {
					logger.Errorf("error on subscribe tracks wrong data format ", req.Data)
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
				case "none":
					log.Println("switch to high quality")
					client.SetQuality(sfu.QualityNone)
				}
			} else if req.Type == TypeUpdateBandwidth {
				bandwidth := uint32(req.Data.(float64))
				client.UpdatePublisherBandwidth(bandwidth)
			} else if req.Type == TypeSetBandwidthLimit {
				bandwidth, _ := strconv.ParseUint(req.Data.(string), 10, 32)
				client.SetReceivingBandwidthLimit(uint32(bandwidth))
			} else if req.Type == TypeIsAllowRenegotiation {
				resp := Respose{
					Status: true,
					Type:   TypeAllowRenegotiation,
					Data:   client.IsAllowNegotiation(),
				}

				respBytes, _ := json.Marshal(resp)

				conn.Write(respBytes)

			} else if req.Type == "start_recording" {
				r.StartRecording(client.ID() + ".webm")
			} else if req.Type == "stop_recording" {
				r.StopRecording()
			} else {
				logger.Errorf("unknown message type", req)
			}
		}
	}
}
