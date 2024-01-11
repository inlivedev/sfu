# Signal Negotiation
After added client to the room, the client can initiate the connection with the SFU server, the client and the SFU server need to exchange signal to be able establish the connection. The signal is used to exchange information about the client and the server. The signal in WebRTC is using SDP (Session Description Protocol) and ICE (Interactive Connectivity Establishment) to exchange information. The information is contain the client's and the server's IP address, port, and other information like the client's and the server's media capabilities.

## Perfect Negotation
The renegotiation can be happen from both side, either from the client or from the SFU. There is a possibility that the renegotiation conflicted when both side is trying to renegotiate at the same time. For example, when a client is adding a new track and the SFU is trying to renegotiate with the client because another client is adding a new track.

To avoid the renegotiation conflict, there is  a mechanism to handle this called [perfect negotiation](https://developer.mozilla.org/en-US/docs/Web/API/WebRTC_API/Perfect_negotiation). The idea is to make sure that the client will only renegotiate with the SFU if the SFU is not trying to renegotiate with the client. If the SFU is trying to renegotiate with the client, then the client will wait until the SFU is done renegotiating with the client, then the client will renegotiate with the SFU. We will cover this in the flow below.

## The negotiation and renegotiation flow
### Initial connection
To initiate a connection, the client will start by doing negotiation with the SFU. To do that this is the flow:
1. Client required to listen for some events from SFU first. Some events will need to respond the callback in order to make the flow complete. The events are: 
   1. `client.OnRenegotiation` callback for when SFU is trying to renegotiate with the client. This will triggered when a track is added or removed from the SFU.
   2. `client.OnAllowedRemoteNegotation` callback for when SFU is done renegotiating with the client. This will triggered when the SFU is done renegotiating with the client. And it a signal that the client can do any renegotiation with the SFU if needed.
   3. `client.OnTracksAdded` callback for when a client add track, and received by the SFU, the client need to confirmed the source of the track is it a media or screen.
   4. `client.OnTrackAvailable` callback to let the client know that a track is available to be added to the peer connection. The client can subscribe the track by calling `client.SubscribeTracks(tracks)` and pass the tracks to subscribe.
   5. `client.OnIceCandidate` callback for when a client received an ICE candidate from the SFU. The client need to add the ICE candidate to the peer connection.
   6. `client.OnConnectionStateChange` callback for when a client connection state is changed. The client need to check if the connection state is connected, then the client can start the renegotiation.

2. The negotation will start with client generate an offer and it will send it to the SFU. You need to add the track to the peer connection before generating the offer. Or you can [add transceiver](https://developer.mozilla.org/en-US/docs/Web/API/RTCPeerConnection/addTransceiver) with `recvonly` direction to the peer connection if you're not publish any media tracks.
3. The offer can be send to server through any protocol like web socket or REST API or any other protocol. On the server, the offer can be added to the SFU by calling `client.Negotiate(offer)`. The method will return SDP answer that need to passback to the client.
4. When the offer is added to the SFU, the SFU will start generate the ice candidate and trigger the `client.OnIceCandidate` callback. That we listen previously.
5. Then just wait until the client is connected. On the client side, this can be done by listening to `peerConnection.addEventListener("connectionstatechange", (event) => {})` event. 
6. When a new track or more added during this first signal negotiation, the tracks won't be available to other clients until client set the source type of the tracks. To set the source, the SFU will trigger `client.OnTracksAdded` callback with the tracks information that we just added through signal negotiation. The client need to confirm the source of the track is it a media or screen by calling `client.SetTrackSourceType()`. The source can be `media` or `screen`. The track ID can be get from the track object, `track.ID()`. The same callback will triggered each time the client add a new track.

### Adding or remove a track
When a client is adding a new track after connection is established, for example when adding a screen sharing. Then the client will need to renegotiate with the SFU. This where the perfect negotiation pattern is used. The renegotiation flow will be like this:
1. Add or remove the track from the peer connection.
2. Check if the SFU is not trying to renegotiate with the client by calling `client.IsAllowNegotiation()`. If it returns true, then the client can start the renegotiation. If it returns false, then the client will wait until the SFU is done renegotiating with the client.
3. If the client is allowed to renegotiate, then the client will generate an offer and send it to the SFU. The offer can be added to the SFU by calling the same method on initiate the connection, `client.Negotiate(offer)`. The method will return SDP answer that need to passback to the client.
4. When the method `client.IsAllowNegotiation()` is return false, it means the SFU currently trying to renegotiate a change with the client, then we we should mark that renegotiation is needed. Then we can wait for the event `client.OnAllowedRemoteNegotation()` to be triggered and do renegotiation again.

## Next
- [Publishing media tracks](./publishing-media.md)