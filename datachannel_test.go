package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestRoomDataChannel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test-room-datachannel", Options{
		WebRTCPort:               40010,
		ConnectRemoteRoomTimeout: 30 * time.Second,
		IceServers:               DefaultTestIceServers(),
	})

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	pc1, client1, _ := CreateDataPair(ctx, testRoom, roomManager.options.IceServers, "peer1")
	pc2, client2, _ := CreateDataPair(ctx, testRoom, roomManager.options.IceServers, "peer2")

	defer func() {
		_ = client1.Stop()
		_ = client2.Stop()
	}()

	chatChan := make(chan string)

	var onDataChannel = func(d *webrtc.DataChannel) {
		if d.Label() == "chat" {
			t.Log("data channel opened ", d.Label())

			d.OnMessage(func(msg webrtc.DataChannelMessage) {
				chatChan <- string(msg.Data)
				if string(msg.Data) == "hello" {
					d.Send([]byte("world"))
				}
			})

			if d.ReadyState() == webrtc.DataChannelStateOpen {
				d.Send([]byte("hello"))
			} else {
				d.OnOpen(func() {
					d.Send([]byte("hello"))
				})
			}
		}
	}

	pc1.OnDataChannel(onDataChannel)

	pc2.OnDataChannel(onDataChannel)

	connected := WaitConnected(ctx, []*webrtc.PeerConnection{pc1, pc2})

	isConnected := <-connected
	require.True(t, isConnected)

	err = testRoom.CreateDataChannel("chat", DefaultDataChannelOptions())
	require.NoError(t, err)

	// make sure to return error on creating data channel with same label
	err = testRoom.CreateDataChannel("chat", DefaultDataChannelOptions())
	require.Error(t, err)

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	messages := ""

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case chat := <-chatChan:
			messages += chat
			glog.Info("chat: ", messages)
			if messages == "hellohelloworldworld" {
				break Loop
			}
		}
	}

	require.Equal(t, "hellohelloworldworld", messages)
}

func TestRoomDataChannelWithClientID(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test-room-datachannel", Options{
		WebRTCPort:               40011,
		ConnectRemoteRoomTimeout: 30 * time.Second,
		IceServers:               DefaultTestIceServers(),
	})

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)
	pc1, client1, _ := CreateDataPair(ctx, testRoom, roomManager.options.IceServers, "peer1")
	pc2, client2, _ := CreateDataPair(ctx, testRoom, roomManager.options.IceServers, "peer2")
	pc3, client3, _ := CreateDataPair(ctx, testRoom, roomManager.options.IceServers, "peer2")

	defer func() {
		_ = client1.Stop()
		_ = client2.Stop()
	}()

	chatChan := make(chan string)

	var onDataChannel = func(d *webrtc.DataChannel) {
		if d.Label() != "chat" {
			return
		}

		t.Log("data channel opened ", d.Label())

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			chatChan <- string(msg.Data)
			if string(msg.Data) == "hello" {
				d.Send([]byte("world"))
			}
		})

		if d.ReadyState() == webrtc.DataChannelStateOpen {
			d.Send([]byte("hello"))
		} else {
			d.OnOpen(func() {
				d.Send([]byte("hello"))
			})
		}
	}

	pc1.OnDataChannel(onDataChannel)

	pc2.OnDataChannel(func(d *webrtc.DataChannel) {
		if d.Label() != "chat" {
			return
		}

		t.Log("data channel opened ", d.Label())

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			chatChan <- string(msg.Data)
			if string(msg.Data) == "hello" {
				d.Send([]byte("noworld"))
			}
		})

		if d.ReadyState() == webrtc.DataChannelStateOpen {
			d.Send([]byte("hello"))
		} else {
			d.OnOpen(func() {
				d.Send([]byte("noworld"))
			})
		}
	})

	pc3.OnDataChannel(onDataChannel)

	connected := WaitConnected(ctx, []*webrtc.PeerConnection{pc1, pc2, pc3})

	isConnected := <-connected
	require.True(t, isConnected)

	err = testRoom.CreateDataChannel("chat", DataChannelOptions{
		Ordered:   true,
		ClientIDs: []string{client1.ID(), client3.ID()},
	})

	require.NoError(t, err)

	// make sure to return error on creating data channel with same label
	err = testRoom.CreateDataChannel("chat", DefaultDataChannelOptions())

	require.Error(t, err)

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	messages := ""

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case chat := <-chatChan:
			messages += chat
			glog.Info("chat: ", messages)
			if messages == "hellohelloworldworld" {
				break Loop
			}
		}
	}

	require.Equal(t, "hellohelloworldworld", messages)
}

// TODO
func TestStillUsableAfterReconnect(t *testing.T) {

}
