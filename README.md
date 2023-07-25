# inLive Hub SFU

This Golang package is an SFU library based on Pion WebRTC. It is designed to be used in the inLive Hub project, but it can also be used in other projects. The goal is to have a Golang SFU library that is portable but easy to extend.

## Components:
This SFU package has 4 components:

1. Client, a WebRTC client as a relay client in the SFU.
2. SFU, the SFU server.
3. Room, a signaling controller between SFU and clients.
4. Room Manager, a controller to manage multiple rooms

## How to use
The one that you will interact with will be the room and the room manager. A server at least will have a single room manager. Before being able to run a group video call, you need to create a room with the room manager, then use that room to add clients to it. You can use any protocol as your signaling controller. 

SFU <-----> Room <-----> REST/WebSocket/gRPC
 |
 |
 \/
 Clients

 Check the [example folder](./examples/) to see how to write a group video call app with this SFU package. The next section will explain in detail how to use this package.


### Connect to the SFU
On the first connection to SFU, a client will do a WebRTC negotiation, exchanging SDP and ice candidates. The client will send an offer SDP to initiate the negotiation. The SFU will respond with an answer SDP. Then both will exchange the ice candidates. 

To initiate the negotiation with the client, the steps will be like this.
1. Add a client first to the SFU by calling [AddClient(clientID, sfu.DefaultClientOptions()[)](./room.go#L133) on the current room instance. The `clientID` must be a unique string. The client options can be customized, but the default should be enough.
2. Once added, now we can access the relay client from the SFU through `currentRoom``.`SFU.Clients[clientID]`. The relay client is a WebRTC client on the SFU side that will be used to negotiate with the client.
3. To start the negotiation you can pass the offer SDP from the client to [client.Negotiate(offer)](./client.go#L113) method. The method will return an answer SDP that you need to pass back to the client.
As part of the negotiation, you need to pass the ice candidates from the client to [client.AddICECandidate(candidate)](./client.go#L353) method. 
5. You also need to pass the ice candidates from the relay client to the client. You can get the ice candidates from the relay client by listening to the `client.OnICECandidate` event. The event will be triggered when the relay client has a new ice candidate. You can get the ice candidate from the event data. Then you can pass the ice candidate to the client. To do this, you need to receive it from the client candidate channel. To access the relay candidate channel, you can call [currentRoom.GetCandidateChan(clientID)](./room.go#L267) method. The method will return a channel that will receive the ice candidate from the relay client. Once you receive it you can pass the ice candidate to the client.

Once you complete the 5 steps above, the connection should be established.

### Renegotiation
Usually, renegotiation will need when a new track is added or removed. The renegotiation can be initiated by the SFU if a new client is joined the room and publish a new track to the room. To broadcast the track to all clients, the SFU must renegotiate with each client to let them know about the new tracks. 

#### Renegotiation from the SFU
To wait for renegotation from SFU to make sure you receive a new track once published in a room, you can do this:
1. Get the relay offer channel from the room specific for the client by calling [currentRoom.GetOfferChan(clientID)](./room.go#L258) method. The method will return a channel that will receive the offer SDP from the SFU if there is a renegotiation from the SFU for that client.
2. Once you receive an offer from the SFU, you can pass the offer SDP to the client and get the answer to pass back to the SFU. 
3. The answer SDP will pass through the relay answer channel that you can get by calling [currentRoom.GetAnswerChan(clientID)](./room.go#L249) method. The method will return a channel that will receive the answer SDP from the client to complete the renegotiation.

#### Renegotiation from the client
The renegotiation also can be initiated by the client if the client is adding a new track, for example by doing a screen sharing. Because both sides can initiate the renegotiation, there is a possibility of both sides are trying to renegotiate at the same time. 

To prevent this, we need to check with the SFU relay client if it is allowed to do renegotiation from the client side by checking the [client.IsAllowNegotiation()](./client.go#L103) method. If it returns true, then we can start the renegotiation by calling [client.Negotiate(offer)](./client.go#L113) method. The same method that we use for the first negotiation. The SFU will respond with an answer SDP to pass back to the client. Make sure after you call [client.IsAllowNegotiation()](./client.go#L1o3) method, you also call [client.Negotiate(offer)](./client.go#L113) method to make sure the in-renegotiation state is processed by the SFU. If you do not call the following method, it will make the SFU think that the client is doing a renegotiation and won't initiate the renegotiation.