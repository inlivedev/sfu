package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
)

func TestRoomDataChannel(t *testing.T) {
	t.Parallel()

	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test", Options{
		EnableMux:                true,
		EnableBandwidthEstimator: true,
		IceServers:               []webrtc.ICEServer{},
	})

	defer roomManager.Close()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)

	err = testRoom.CreateDataChannel("chat", DefaultDataChannelOptions())
	require.NoError(t, err)

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

	_, client1, _, connChan1 := CreateDataPair(ctx, TestLogger, testRoom, roomManager.options.IceServers, "peer1", onDataChannel)
	_, client2, _, connChan2 := CreateDataPair(ctx, TestLogger, testRoom, roomManager.options.IceServers, "peer2", onDataChannel)

	defer func() {
		_ = testRoom.StopClient(client1.id)
		_ = testRoom.StopClient(client2.id)
	}()

	timeoutConnected, cancelTimeoutConnected := context.WithTimeout(ctx, 40*time.Second)
	defer cancelTimeoutConnected()
	isConnected := false

	connectedCount := 0
LoopConnected:
	for {
		select {
		case <-timeoutConnected.Done():
			cancelTimeoutConnected()
			t.Fatal("timeout waiting for connected")
		case state1 := <-connChan1:
			if state1 == webrtc.PeerConnectionStateConnected {
				connectedCount++
			}

			if connectedCount == 2 {
				isConnected = true
				break LoopConnected
			}
		case state2 := <-connChan2:
			if state2 == webrtc.PeerConnectionStateConnected {
				connectedCount++
			}

			if connectedCount == 2 {
				isConnected = true
				break LoopConnected
			}
		}
	}

	require.True(t, isConnected)

	// make sure to return error on creating data channel with same label
	err = testRoom.CreateDataChannel("chat", DefaultDataChannelOptions())
	require.Error(t, err)

	timeout, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	messages := ""
	expectedMessages := "hellohelloworldworld"

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case chat := <-chatChan:
			messages += chat
			t.Log("chat: ", messages)
			if len(messages) == len(expectedMessages) {
				break Loop
			}
		}
	}

	require.Equal(t, len(expectedMessages), len(messages))
}

func TestRoomDataChannelWithClientID(t *testing.T) {
	t.Parallel()

	report := CheckRoutines(t)
	defer report()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test", Options{
		EnableMux:                true,
		EnableBandwidthEstimator: true,
		IceServers:               []webrtc.ICEServer{},
	})

	defer roomManager.Close()

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	roomOpts := DefaultRoomOptions()
	roomOpts.Codecs = []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus}
	testRoom, err := roomManager.NewRoom(roomID, roomName, RoomTypeLocal, roomOpts)
	require.NoError(t, err, "error creating room: %v", err)

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

	onDataChannel2 := func(d *webrtc.DataChannel) {
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
	}

	_, client1, _, connChan1 := CreateDataPair(ctx, TestLogger, testRoom, roomManager.options.IceServers, "peer1", onDataChannel)
	_, client2, _, connChan2 := CreateDataPair(ctx, TestLogger, testRoom, roomManager.options.IceServers, "peer2", onDataChannel2)
	_, client3, _, connChan3 := CreateDataPair(ctx, TestLogger, testRoom, roomManager.options.IceServers, "peer2", onDataChannel)

	defer func() {
		_ = testRoom.StopClient(client1.id)
		_ = testRoom.StopClient(client2.id)
		_ = testRoom.StopClient(client3.id)
	}()

	timeoutConnected, cancelTimeoutConnected := context.WithTimeout(ctx, 30*time.Second)

	defer cancelTimeoutConnected()

	isConnected := false

	connectedCount := 0

LoopConnected:
	for {
		select {
		case state1 := <-connChan1:
			if state1 == webrtc.PeerConnectionStateConnected {
				connectedCount++
			}

			if connectedCount == 3 {
				isConnected = true
				break LoopConnected
			}

		case state2 := <-connChan2:
			if state2 == webrtc.PeerConnectionStateConnected {
				connectedCount++
			}

			if connectedCount == 3 {
				isConnected = true
				break LoopConnected
			}

		case state3 := <-connChan3:
			if state3 == webrtc.PeerConnectionStateConnected {
				connectedCount++
			}

			if connectedCount == 3 {
				isConnected = true
				break LoopConnected
			}

		case <-timeoutConnected.Done():
			cancelTimeoutConnected()
			t.Fatal("timeout waiting for connected")
		}
	}

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
	expectedMessages := "hellohelloworldworld"

Loop:
	for {
		select {
		case <-timeout.Done():
			break Loop
		case chat := <-chatChan:
			messages += chat
			t.Log("chat: ", messages)
			if len(messages) == len(expectedMessages) {
				break Loop
			}
		}
	}

	require.Equal(t, len(expectedMessages), len(messages))
}

// TODO
func TestStillUsableAfterReconnect(t *testing.T) {

}
