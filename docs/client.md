# Add and remove client from a room
To be able to publish or subscribe to a video stream, the client need to join a room first. To do this the client need to register first.

## Add a client to the room
To add a client, you can call `room.AddClient(clientID)` method. The client ID can be any string. The method will return a client object that can be used to publish or subscribe to a video stream.

```go
// Use the SFU CreateClientID() helper function to generate a unique clientID
clientID := r.CreateClientID()

// add a new client to room
// you can also get the client by using r.GetClient(clientID)
opts := sfu.DefaultClientOptions()
opts.EnableVoiceDetection = true
client, err := room.AddClient(clientID, clientID, opts)
if err != nil {
    if err == sfu.ErrClientExists {
        // client already exists
    } else {
        // error when adding client
    }
}
```

## Remove a client from the room
When you're done with the client and want to disconnect the client from the room, you can stop the client. This will close the connection. All tracks from the client will be unpublished and removed from the room. To stop the client, you do it from the room instance.

```go
room.StopClient(client.ID())
```

## Next
- [Signal negotiation](./signal.md)