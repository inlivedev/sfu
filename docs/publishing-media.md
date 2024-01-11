# Publishing media tracks
One of common use case of SFU is publishing media tracks from a client to the SFU. The media tracks can be audio or video. The client can publish multiple media tracks to the SFU. The SFU will forward the media tracks to other clients that subscribe to the media tracks. 

There are several things that can be done to optimize the media tracks publishing to make sure the other clients can optimally receive the media tracks no matter what the network condition they have. The following are the things that can be done to optimize the media tracks publishing:

## Video tracks
When publishing video tracks it is important to set the proper bitrate of the tracks that we will send. To do this, we will use the bitrate config options when creating the room. As we know from [room documentation](./room.md), we can set the bitrate config options when creating the room. We can configure the custom bitrate like this:

```go
// create new room
roomsOpts := sfu.DefaultRoomOptions()
roomsOpts.BitrateConfig.VideoHigh = 1_200_000
roomsOpts.BitrateConfig.VideoMid = 500_000
roomsOpts.BitrateConfig.VideoLow = 150_000

room, _ := roomManager.NewRoom(roomID, roomName, sfu.RoomTypeLocal, roomsOpts)
```

The bitrate config options above will set the maximum bitrate for each video quality in bit per second(bps). Then we can get the current bitrates config of the room with `room.BitrateConfigs()` and pass it to the client to configure the max bitrate of the video tracks that will be published to the SFU.

When the client ingress bandwidth is less than 1.2 mbps, then the SFU will adjust the bitrate to send mid quality because the client bandwidth is not enough to receive high quality. The same thing will happen when the client ingress bandwidth is less than 500 kbps, then the SFU will adjust the bitrate to send low quality. This way, the client will always receive the video tracks with the most optimal quality based on the client bandwidth.

### Simulcast
Simulcast is a way to send multiple video quality in different track. The SFU will receive the simulcast track from the client and but only send one track to the other clients. The SFU will choose the most optimal stream to send to the other clients based on the client network condition. inLive SFU is support simulcast using H264 codec. This can be a good option to use if you're consider the efficient CPU usage. 

This is how to enable simulcast when publishing video tracks:

```js
// get user media stream with custom constraints to achieve HD quality
const stream = await navigator.mediaDevices.getUserMedia({video: {
                width: {ideal: 1280}, 
                height: {ideal: 720}, 
                advanced: [{
                    frameRate: {min: 30}}, 
                    {height: {min: 360}}, 
                    {width: {min: 720}}, 
                    {frameRate: {max: 30}}, 
                    {width: {max: 1280}}, 
                    {height: {max: 720}}, 
                    {aspectRatio: {exact: 1.77778}}
                ]}, audio: true});

// add video track to the peer connection
peerConnection.addTransceiver(stream.getVideoTracks()[0], {
    direction: 'sendonly',
    streams: [stream],
    sendEncodings: [
            {
                rid: 'high',
                maxBitrate: 1200*1000,
                maxFramerate: 30,
            },
            {
                rid: 'mid',
                scaleResolutionDownBy: 2.0,
                maxFramerate: 30,
                maxBitrate: 500*1000,
            },
            {
                rid: 'low',
                scaleResolutionDownBy: 4.0,
                maxBitrate: 150*1000,
                maxFramerate:30,
            }
        ]
    });
```

Once the track added to the peer connection, you can start the negotiation with the SFU. See the [signal negotiation documentation](./signal.md) how to to the negotiation.

We test the simulcast with the H264 codec. Although the VP9 codec is also support simulcast, but we're not properly test it yet. So we recommend to use H264 codec if you want to use simulcast. When doing the simulcast, make sure to set the highest quality resolution to 720p because Chromium browser won't send the video track less than 180 pixels. So if you set the highest quality to 640p, then the mid quality will be 320p and the low quality will be 160p. And the 160p will not be sent to the SFU because it's less than 180 pixels. The simulcast still working, but then you won't get the low quality video track.

### Scalable Video Codec (SVC)
Scalable Video Codec(SVC) is a way to send multiple quality using single track. The SFU will receive the SVC track from the client and will manipulate the track quality before send it to the other clients. The SFU will choose the most optimal quality to send to the other clients based on the client network condition. inLive SFU is support SVC using VP9 codec. This can be a good option to use if you're consider the efficient bandwidth usage.

To do SVC in the client side, you need to set the `scalabilityMode` to `L3T3` when adding the video track to the peer connection. This is how to do it:

```js
peerConnection.addTransceiver(stream.getVideoTracks()[0], {
    direction: 'sendonly',
    streams: [stream],
    sendEncodings: [
            {
                maxBitrate: 1200*1000,
                scalabilityMode: 'L3T3'
            },
            
        ]
    });
```

When using SVC with scalability mode L3T3, it means there are 3 spatial layers and 3 temporal layers. The spatial layers are the resolution of the video track. The temporal layers are the frame rate of the video track. With L3T3 scalability mode, the SFU will have combination of three level quality as below:
- high: 720p, 30fps
- mid: 360p, 15fps
- low: 180p, 7fps

You can make the low layer to be 15fps by set the scalability mode to L3T2, or all will 30 fps by set the scalability mode to L3T1. Or single resolution but different frame rate by set the scalability mode to L1T3. You can read more about scalability mode in [here](https://webrtcglossary.com/svc/).

One thing that we should aware about SVC, we can't set the maximum bitrate for each layer. So bitrate config need to customize to make sure the SFU will send the most optimal quality to the client. To know the bitrate for each quality layer we can use the [example app](../examples/http-websocket/) and check the received bitrate when setting the maximum received bitrate.

## Audio tracks
inLive SFU can receive multiple audio tracks from the client and forward it to the other clients. The supported codec for audio tracks are Opus and Opus RED. Opus RED is a redundant audio track that can be used to make sure the audio track is received by the other clients even if the network condition is not good. The disadvantage is the bandwidth will be used more than the normal Opus audio track. Our test shows that the Opus RED will use 2x more bandwidth than the normal Opus audio track. To use the Opus RED, we need to arrange the codec priority when adding track. This can be done like this:

```js
const audioTcvr= peerConnection.addTransceiver(stream.getAudioTracks()[0], {
    direction: 'sendonly',
    streams: [stream],
    sendEncodings: [{priority: 'high'}],
})



if(audioTcvr.setCodecPreferences != undefined){
    const audioCodecs = RTCRtpReceiver.getCapabilities('audio').codecs;
    
    let audioCodecsPref = [];
    if (red){
        for(let i = 0; i < audioCodecs.length; i++){
            // audio/red 48000 111/111
            if(audioCodecs[i].mimeType == "audio/red"){
                audioCodecsPref.push(audioCodecs[i]);
            }
        }
    }
    
    for(let i = 0; i < audioCodecs.length; i++){
        if(audioCodecs[i].mimeType == "audio/opus"){
            audioCodecsPref.push(audioCodecs[i]);
        }
    }
    
    audioTcvr.setCodecPreferences(audioCodecsPref);
}
```

## Next
- [Subscribe and view video](./video-subscription.md)