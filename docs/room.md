# Create and close a room
Before a client can publish or subscribe to a video stream, the client need to join a room first. A room is a virtual room where clients can join and publish/subscribe to video streams. A client can only publish/subscribe to video streams from the same room.

## Create a room
To create a room, you can call `client.CreateRoom(roomID)` method. The room ID can be any string. The method will return a room object that can be used to add or remove client from the room.

```go
// create room manager first before create new room
ctx:= context.Background()
sfuOpts := sfu.DefaultOptions()
roomManager := sfu.NewManager(ctx, "server-name-here", sfuOpts)

// generate a new room id. 
roomID := roomManager.CreateRoomID()
roomName := "test-room"

// create new room
roomsOpts := sfu.DefaultRoomOptions()
room, _ := roomManager.NewRoom(roomID, roomName, sfu.RoomTypeLocal, roomsOpts)
```

## Close a room
When you're done with the room and want to disconnect all the participants in the room, you can close the room. This will stop all clients in the room. All tracks will also remove from the room before close the room. To close the room, you can do it either from room manager or directly from the room instance.

```go
// close room from room manager
roomManager.CloseRoom(roomID)
```

The code above similar with this code below:

```go
if room,err:= roomManager.GetRoom(roomID);err!=nil{
    room.Close()
}
```

## Next
- [Add and remove client from room](./client.md)