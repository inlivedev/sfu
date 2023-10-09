# Perfect Negotation
Each time a new track is added or removed from a client, the client will need to let the others client know through SFU. To let the other clients knows, the client will need to renegotiate with the SFU so the SFU can know what's changing. The SFU will then renegotiate with each client to inform them about changes. 

But then, when the client is renegotiate with the SFU but in the same time the SFU also want to renegotiate with the client because another client is having a change, then it will be conflict and there should be a mechanism to handle this. 

The mechanism is called perfect negotiation. The idea is to make sure that the client will only renegotiate with the SFU if the SFU is not trying to renegotiate with the client. If the SFU is trying to renegotiate with the client, then the client will wait until the SFU is done renegotiating with the client, then the client will renegotiate with the SFU.

## How it works

### 1. OnRenegotationNeeded 
When a tracks is added or removed from a client, the client will need to renegotiate with the SFU to let the SFU knows about the changes. The client will trigger `PeerConnection.OnNegotationNeeded()` event but it seems the event is not consistent in Pion library. So for now better to do manual renegotation each time we add or remove tracks. By doing this, we can also prevent the event triggered more than it needed. For example when we add a video from camera, it will have two tracks, audio and video. I believe Pion already prevent the event to be triggered twice, but just to make sure, we can do manual renegotation.

### 2. Ask SFU if it is allowed to renegotiate
Before a client is trying to renegotiate with the SFU, the client will check with SFU if it is allowed to do renegotation by calling `client.IsAllowNegotiation()`. If it returns true, then the client can start the renegotiation. If it returns false, then the client will wait until the SFU is done renegotiating with the client.

When we already added or removed the client's track, but the `client.IsAllowNegotiation()` is return false means the SFU currently trying to renegotiate a change with the client, then we we should mark that renegotiation is needed. For example:

```go
var pendingTracks []*webrtc.TrackLocalStaticSample

var pendingNegotation bool

// this will call when the SFU is trying to renegotiate with the client
client.OnRenegotiation = func(offer webrtc.SessionDescription) (webrtc.SessionDescription,error){
  if pendingNegotation {
    // if we have pending renegotiation, it means previously the client already trying to renegotiate with the SFU but it's not allowed.
    // so the SFU will know that we are not ready to renegotiate
    return webrtc.SessionDescription{}, nil
  }
}

// 
//
for tracks = range pendingTracks {
    // add track to peer connection
    peerConnection.AddTrack(track)
}

if !client.IsAllowNegotiation() {
    // mark that renegotiation is needed
   pendingNegotation = true
}


```


1. When the SFU is done with renegotation, it will call `client.OnAllowedRemoteNegotation()`, this should be use to repeat the previous client renegotation that was not allowed before. 
2. When 