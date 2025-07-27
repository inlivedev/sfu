
> [!IMPORTANT]  
> This library still in very early development. The API might change in the future. We use it in production for [inLive Hub project](https://inlive.app/realtime-interactive). We're a small team, so we can't guarantee that we can fix all issues. But we will try our best to fix it especially when it's related with our product. If you have any questions or find any issues, please write it to [this repo Github issue](https://github.com/inlivedev/sfu/issues). We will appreciate it.

# inLive Hub SFU

This Golang package is an SFU library based on Pion WebRTC. It is designed to be used in the [inLive Hub project](https://inlive.app/realtime-interactive), but it can also be used in other projects. The goal is to have a Golang SFU library that is portable but easy to extend.

## Installation
This is a Go module, so you can install it by running this command in your app directory:
```
go get github.com/inlivedev/sfu
```



## Features
- [x] Group call
- [x] Screen sharing
- [x] Perfect negotiation
- [x] Data channel - note: only room channel currenty available
- [x] Simulcast 
- [x] Scalable Video Coding (SVC)
- [x] Voice activity detection
- [x] Room statistic

## Components:
This SFU package has four components:

1. Client, a WebRTC client as a relay client in the SFU.
2. SFU, the SFU server.
3. Room, a signaling controller between SFU and clients.
4. Room Manager, a controller to manage multiple rooms

## How to use
The one that you will interact with will be the room and the room manager. A server will at least have a single room manager. Before being able to run a group video call, you need to create a room with the room manager, then use that room to add clients to it. You can use any protocol as your signaling controller. 
```
SFU <---> Room <---> REST/WebSocket/gRPC <---> browser/app
 |
 |
 \/
 Clients
```

See the [example folder](./examples/) to see how to write a group video call app with this SFU package. The following section will explain in detail how to use this package.


### Connect to the SFU
On the first connection to SFU, a client will do a WebRTC negotiation, exchanging Session Description Protocol(SDP) and ice candidates. The client will send an offer SDP to initiate the negotiation. The SFU will respond with an answer SDP. Then both will exchange the ice candidates. 

The steps will be like this to initiate the negotiation with the client.
1. Add a client first to the SFU by calling [AddClient(clientID, sfu.DefaultClientOptions())](./room.go#L133) on the current room instance. The `clientID` must be a unique string. The client options can be customized, but the default should be enough.
2. Once added, now we can access the relay client from the SFU through `currentRoom.SFU.GetClient(clientID)`. The relay client is a WebRTC client on the SFU side that will be used to negotiate with the client.
3. To start the negotiation you can pass the offer SDP from the client to [client.Negotiate(offer)](./client.go#L113) method. The method will return an answer SDP that you need to pass back to the client.
As part of the negotiation, you need to pass the ice candidates from the client to [client.AddICECandidate(candidate)](./client.go#L353) method. 
4. You also need to pass the ice candidates from the relay client to the client. You can get the ice candidates from the relay client by listening to the `client.OnICECandidate` event. The event will be triggered when the relay client has a new ice candidate. You only need to send this ice candidate to the client.

Once you complete the five steps above, the connection should be established. See the example of how the connection is established [here](./examples/http-websocket/main.go#L91)

### Publish and subscribe tracks
When a client wants to publish a track to the SFU, usually the flow is like this:
1. Get the media tracks from either the camera or the screen sharing. You can use [getUserMedia](https://developer.mozilla.org/en-US/docs/Web/API/MediaDevices/getUserMedia) to get the media tracks.
2. Add the media tracks to the WebRTC PeerConnection instance. You can use [addTrack](https://developer.mozilla.org/en-US/docs/Web/API/RTCPeerConnection/addTrack) to add the media tracks to the PeerConnection instance.
3. This will add the tracks to SFU but won't be published until the client sets the track source type either `media` or `screen`.
4. When the track is added to SFU, the client will trigger `client.OnTrackAdded([]*tracks)`. We need to use this event to set the track source type by calling `client.`SetTracksSourceType(map[trackTypes string]TrackType)`
5. When successful, this will trigger `client.OnTrackAvailable([]*tracks)` event on other clients.
6. Use this event to subscribe to the tracks by calling `client.SubscribeTracks(tracksReq []SubscribeTrackRequest)` method. When successful, it will trigger `peerConnection.OnTrack()` event that we can use to attach the track to the HTML video element. Or instead of manually subscribing to each track, you can also use `client.SubscribeAllTracks()` method to subscribe to all available tracks. You only need to call it once, and all available tracks will automatically added to the PeerConnection instance.

See the [client_test.go](./client_test.go) for more details on how to publish and subscribe to tracks.


### Client Renegotiation
Usually, renegotiation will be needed when a new track is added or removed. The renegotiation can be initiated by the SFU if a new client joins the room and publishes a new track to the room. To broadcast the track to all clients, the SFU must renegotiate with each client to inform them about the new tracks. The renegotiation is can be handled in peerConnection.OnNegotiationNeeded event. This event will be triggered when the client needs to renegotiate with the SFU.

> [!IMPORTANT]
> When adding a track and using the `OnNegotiationNeeded` event, issues may arise if you previously added a single `recvOnly` transceiver and then add more than one `sendonly` or `sendrecv` transceiver. The `OnNegotiationNeeded` event may be triggered multiple times. To prevent this, follow these rules:
> - If you add a `recvonly` transceiver and want to add more tracks, use `PeerConnection.AddTrack()` so the send direction of the existing transceiver will be used.
> - Always balance the number of transceivers by adding the same number of additional tracks as the `recvonly` transceivers previously added. For example, if you added 2 `recvonly` transceivers, you can add 2 tracks with `PeerConnection.AddTrack()`. Adding more than 2 tracks will cause `OnNegotiationNeeded` to be called multiple times. It is common to add `recvonly` transceivers first for audio and video, then add more tracks later when the client wants to share the screen or other media.
  
#### Renegotiation from the SFU
To wait for the renegotiation process from SFU to make sure you receive a new track once published in a room, you can do this:
1. When creating a new client after calling `currentRoom.addClient()` method, you can listen to the `client.OnRenegotiation` event. The event will be triggered when the SFU is trying to renegotiate with the client.
2. Once you receive an offer from the SFU, you can pass the offer SDP to the client and get the answer to pass back to the SFU. 
3. The event `client.OnRenegotiation` must return an answer SDP received from the client, and it suggested having a timeout when waiting for the answer from the client. If the timeout is reached, you can return an empty  SDP to the SFU. 

See the example of how the renegotiation is handled [here](./examples/http-websocket/main.go#L113)

#### Renegotiation from the client
The renegotiation also can be initiated by the client if the client is adding a new track, for example, by doing a screen sharing. Because both sides can initiate the renegotiation, there is a possibility that both sides are trying to renegotiate at the same time. 

To prevent this, we need to check with the SFU relay client if it is allowed to do renegotiation from the client side by checking the [client.IsAllowNegotiation()](./client.go#L103) method. If it returns true, then we can start the renegotiation by calling [client.Negotiate(offer)](./client.go#L113) method. The same method that we used for the first negotiation. The SFU will respond with an SDP answer to the client. Make sure after you call [client.IsAllowNegotiation()](./client.go#L1o3) method, you also call [client.Negotiate(offer)](./client.go#L113) method to make sure the in-renegotiation state is processed by the SFU. If you do not call the following method, the SFU will think that the client is doing a renegotiation and won't initiate the renegotiation.

If the `client.IsAllowNegotiation()` is returned false, it means the SFU is currently trying to renegotiate with the client. So the client should wait until the SFU is done renegotiating with the client. In the browser, you can listen to the `onnegotiationneeded` event to know when the SFU is done renegotiating with the client.

### Leave the room
If a client wants to leave a room or disconnect from the SFU. You can [close the PeerConnection](https://developer.mozilla.org/en-US/docs/Web/API/RTCPeerConnection/close) instance on the client side. The SFU will detect that the client is closed and will remove the client from the room.

## Logging
This library is using Glog for logging. You can set the log level by setting the `-flag stderrthreshold=warning` when running an application from the command line. You can control it also from your app using the [flag package](https://pkg.go.dev/flag). The default log level is `info`. The available log levels are `info`, `warning`, `error`, `fatal`, and `panic`. You can read more about Glog [here](https://pkg.go.dev/github.com/golang/glog)

To set the log printing to stderr instead of a file, you can set the `-flag logtostderr=true` flag variable to `true`. The default is `false`.

## Licence
MIT License - see [LICENSE](./LICENSE) for the full text