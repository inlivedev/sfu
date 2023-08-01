package sfu

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoomCreateAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create room manager first before create new room
	roomManager := NewManager(ctx, "test", Options{})

	roomID := roomManager.CreateRoomID()
	roomName := "test-room"

	// create new room
	testRoom := roomManager.NewRoom(roomID, roomName, RoomTypeLocal)

	// add a new client to room
	// you can also get the client by using r.GetClient(clientID)
	client1, err := testRoom.AddClient(testRoom.CreateClientID(testRoom.GetSFU().Counter), DefaultClientOptions())
	require.NoErrorf(t, err, "error adding client to room: %v", err)

	// stop client
	err = testRoom.StopClient(client1.ID)
	require.NoErrorf(t, err, "error stopping client: %v", err)

	client2, err := testRoom.AddClient(testRoom.CreateClientID(testRoom.GetSFU().Counter), DefaultClientOptions())
	require.NoErrorf(t, err, "error adding client to room: %v", err)

	// stop all clients should error on not empty room
	err = testRoom.Close()
	require.EqualError(t, err, ErrRoomIsNotEmpty.Error(), "expecting error room is not empty: %v", err)

	// stop other client
	err = testRoom.StopClient(client2.ID)
	require.NoErrorf(t, err, "error stopping client: %v", err)

	err = testRoom.Close()
	require.NoErrorf(t, err, "error closing room: %v", err)
}
