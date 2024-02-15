package sfu

import (
	"context"
	"errors"
	"io"
	"path"
	"runtime"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestSVCPacketDropSequence(t *testing.T) {
	t.Parallel()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	ctx := testRoom.sfu.context

	_, _, trackChanReceiver, connected := createPeerSVC(ctx, testRoom, []webrtc.ICEServer{}, "sender", false)
	createPeerSVC(ctx, testRoom, []webrtc.ICEServer{}, "receiver", false)

	timeout, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	defer cancelTimeout()
	var state bool

	select {
	case state = <-connected:
	case <-timeout.Done():
		require.Fail(t, "timeout waiting for connection")
		return
	}

	if !state {
		require.Fail(t, "sender not connected")
		return
	}

	select {
	case <-timeout.Done():
		require.Fail(t, "timeout waiting for track")
		return
	case track := <-trackChanReceiver:
		lastSeq := uint16(0)
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				if err == io.EOF {
					return
				}

				glog.Info("error reading rtp packet: ", err.Error())
				return
			}

			if lastSeq != 0 && pkt.SequenceNumber != lastSeq+1 {
				glog.Info("test: packet drop ", pkt.SequenceNumber, " last ", lastSeq)
			}

			lastSeq = pkt.SequenceNumber
		}
	}
}

func createPeerSVC(ctx context.Context, room *Room, iceServers []webrtc.ICEServer, peerName string, isLoop bool) (*webrtc.PeerConnection, *Client, chan *webrtc.TrackRemote, chan bool) {
	var (
		client      *Client
		mediaEngine *webrtc.MediaEngine = GetMediaEngine()
	)

	i := &interceptor.Registry{}

	// Use the default set of Interceptors
	_ = webrtc.RegisterDefaultInterceptors(mediaEngine, i)

	webrtcAPI := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(i))

	pc, _ := webrtcAPI.NewPeerConnection(webrtc.Configuration{})

	connected := make(chan bool)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connected <- true
		case webrtc.PeerConnectionStateDisconnected:
			connected <- false
		case webrtc.PeerConnectionStateFailed:
			connected <- false
		}
	})

	trackChan := make(chan *webrtc.TrackRemote)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		trackChan <- track
	})

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("No caller information")
	}

	videoFileName := path.Join(path.Dir(filename), "./media/vp9/output-vp9-1.ivf")

	LoadVp9Track(ctx, pc, videoFileName, isLoop)

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	id := room.CreateClientID()
	client, _ = room.AddClient(id, id, DefaultClientOptions())
	client.SubscribeAllTracks()

	client.OnAllowedRemoteRenegotiation(func() {
		glog.Info("allowed remote renegotiation")
		negotiate(pc, client)
	})

	client.OnIceCandidate(func(ctx context.Context, candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		_ = pc.AddICECandidate(candidate.ToJSON())
	})

	client.OnRenegotiation(func(ctx context.Context, offer webrtc.SessionDescription) (answer webrtc.SessionDescription, e error) {
		if client.state.Load() == ClientStateEnded {
			glog.Info("test: renegotiation canceled because client has ended")
			return webrtc.SessionDescription{}, errors.New("client ended")
		}

		currentTranscv := len(pc.GetTransceivers())

		glog.Info("test: got renegotiation ", peerName)
		defer glog.Info("test: renegotiation done ", peerName)
		if err := pc.SetRemoteDescription(offer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		answer, _ = pc.CreateAnswer(nil)

		if err := pc.SetLocalDescription(answer); err != nil {
			return webrtc.SessionDescription{}, err
		}

		newTcv := len(pc.GetTransceivers()) - currentTranscv
		glog.Info("test: new transceiver ", newTcv, " total tscv ", len(pc.GetTransceivers()))

		return *pc.LocalDescription(), nil
	})

	negotiate(pc, client)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		client.PeerConnection().PC().AddICECandidate(candidate.ToJSON())
	})

	return pc, client, trackChan, connected
}

func sendPackets(ctx context.Context, track *webrtc.TrackLocalStaticRTP) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := track.WriteRTP(&rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i),
					SSRC:           1,
				},
				Payload: []byte{0x00},
			}); err != nil {
				glog.Error("error writing rtp packet: ", err.Error())
				return
			}
		}

		i++

	}
}
