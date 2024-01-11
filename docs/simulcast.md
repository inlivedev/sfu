# Simulcast
Simulcast is a way to send multiple video streams with different quality. The SFU will receive the simulcast stream from the client and but only send one stream to the other clients. The SFU will choose the most optimal stream to send to the other clients based on the client condition.

## Decided which quality to send to the client
Sending the most optimal quality for the client is to make sure the client can play the media stream smoothly no matter what the client condition is. To decide which quality to send to the client, there are two things that we need to consider:

### 1. The client bandwidth
The client bandwidth is the most important thing to consider when deciding which quality to send to the client. If the client bandwidth is low, then we need to send a lower quality stream to the client. If the client bandwidth is high, then we can send a higher quality stream to the client. WebRTC already come with bandwidth estimator that can estimate the client bandwidth. We can use the bandwidth estimator to decide which quality to send to the client. The available bandwidth is available in [RTCIceCandidatePairStats](https://www.w3.org/TR/webrtc-stats/#dom-rtcicecandidatepairstats) that can be accessed from [RTCPeerConnection.GetStats()](https://pkg.go.dev/github.com/pion/webrtc/v3#PeerConnection.GetStats).
### 2. How the video stream played on the screen
The video stream that receive by the client is not always visible by the user. For example, when too many participants means there will be too many video streams that need to play but the screen layout is not enough to show all the video streams. For example in presentation mode, the screen sharing will be bigger than the other video streams. And the other video streams will play in a small size, and some of it might be invisble because it's not in the screen layout. With this condition sending a bigger resolution video stream and played in a small video player is not efficient. We need to inform the SFU about the video player size on the screen so the SFU is not send the bigger resolution video stream to the client.

On the web, we can detect the video player size changes by listening to the [ResizeObserver](https://developer.mozilla.org/en-US/docs/Web/API/ResizeObserver) event. When the video player size changes, we can send the new video player size to the SFU.

On the SFU side, the decision will be made based on the following steps:
1. The simulcast stream will be received by the using these three simulcast streams: low, medium, and high. Each will be sent with different resolution and bitrate. For example the combination of resolution and bitrates can be like this:
   - Low: 240p, 300kbps
   - Mid: 480p, 800kbps
   - High: 720p, 1700kbps
2. The size of the video player on the screen layout will be the maximum size of the video stream that will be sent to the client. The maximum quality that can be sent to the client is the closest quality below the maximum size of the video player. For example, if the maximum size of the video player is 512x384, then the maximum quality that can be sent to the client is 480p.
3. If the client bandwidth is lower than the maximum size of the video player, then the SFU set the maximum quality of stream to sent to the client will based on the bandwidth. For example if the client bandwidth is 500kbps, then the maximum size of the video player is 240p even previously the video player size is bigger.


### 3. Only stream the video if the video player is visible in the screen layout
When the video player is not visible in the screen layout, then we should not stream the video to the client. This is to make sure that the client bandwidth is not wasted to stream the video that is not visible by the user. To do this, we need to inform the SFU if the video player is switch the visibility state in the screen layout.

To detect the visibliity of the video, we can use the [IntersectionObserver](https://developer.mozilla.org/en-US/docs/Web/API/Intersection_Observer_API) to detect if the video player is visible in the screen layout. When the video player visibility is changes, we can send the new visibility state to the SFU so the SFU can pause the video track to save the client bandwidth.

This is how it can be done with JavaScript:
```js

// create the observer
const observer = new IntersectionObserver((entries) => {
  // loop through the entries
  entries.forEach((entry) => {
    // if the entry is visible
    if (entry.isIntersecting) {
      // send the visibility state to the SFU
      sendVisibilityState(true)
    } else {
      // send the visibility state to the SFU
      sendVisibilityState(false)
    }
  })
})

// observe the video player
observer.observe(videoPlayer)

```
