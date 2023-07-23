package sfu

import (
	"testing"
)

func TestBroadcastDataChannel(t *testing.T) {
	// ctx, cancel := context.WithCancel(context.Background())
	// defer cancel()
	// udpMux := NewUDPMux(ctx, 40008)

	// connectedChan := make(chan bool)

	// turn := TurnServer{
	// 	Port:     3478,
	// 	Host:     "turn.inlive.app",
	// 	Username: "inlive",
	// 	Password: "inlivesdkturn",
	// }

	// sfu := New(ctx, turn, udpMux)

	// tracks, mediaEngine := testhelper.GetTestTracks()
	// defer sfu.Stop()

	// peer1, _ := createPeer(ctx, t, sfu, tracks, mediaEngine, connectedChan)
	// chatChannel := "chat"
	// dataChannel, _ := peer1.CreateDataChannel(chatChannel, nil)
	// chatChan := make(chan Data)

	// dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
	// 	log.Println("data channel message: ", string(msg.Data))
	// 	chat := Data{}
	// 	json.Unmarshal(msg.Data, &chat)
	// 	chatChan <- chat
	// })

	// dataChannel.OnOpen(func() {
	// 	dataChannel.Send([]byte("hello"))
	// })

}

// func TestPrivateDataChannel(t *testing.T) {
// 	receiver.PeerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
// 		require.Equal(t, "pm-"+peers[0].ID, d.Label())

// 		d.OnMessage(func(msg webrtc.DataChannelMessage) {
// 			require.Equal(t, "hello", string(msg.Data))
// 			msgReceived <- true
// 		})
// 	})

// 	dcPM2, err := sender.PeerConnection.CreateDataChannel("pm-"+peers[2].ID, nil)

// 	require.NoError(t, err)

// 	relay := sfu.Clients[sender.ID]
// 	if relay.IsAllowNegotiation() {
// 		offer, _ := sender.PeerConnection.CreateOffer(nil)
// 		_ = sender.PeerConnection.SetLocalDescription(offer)
// 		answer, _ := relay.Negotiate(*sender.PeerConnection.LocalDescription())
// 		_ = sender.PeerConnection.SetRemoteDescription(*answer)
// 	}

// 	dcPM2.OnOpen(func() {
// 		dcPM2.Send([]byte("hello"))
// 	})

// 	ctxTimeoutMsg, cancelMsg := context.WithTimeout(ctx, 30*time.Second)
// 	defer cancelMsg()

// 	select {
// 	case <-ctxTimeoutMsg.Done():
// 		require.Fail(t, "timeout waiting the message")
// 	case <-msgReceived:
// 	}
// }
