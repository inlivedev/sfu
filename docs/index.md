# Documentation
This documentation is about mandatories functionality that part of the SFU. You can learn how we implement the feature from each documentation and how to use it in your own app. 

## Glossary
- **SFU**: Selective Forwarding Unit. A server that receives a video stream from a client and forwards it to other clients. The SFU can choose which video stream to forward to other clients.
- **Client**: A client that connects to the SFU. The client can publish a video stream and subscribe to other video streams.
- **Publisher**: A client that publishes a video stream to the SFU.
- **Subscriber**: A client that subscribes to a video stream from the SFU.
- **Track**: An audio/video stream that published by the client. A client can publish multiple tracks.
- **Room** A virtual room where clients can join and publish/subscribe to video streams. A client can only publish/subscribe to video streams from the same room.


## How it works
The SFU basic function is to receive media stream from a client and forward it to other clients. There is a mechanism in SFU that optimizing how the stream is forwarded to other clients to make sure the receiver clients can play the stream smoothly. 

## Documentation
- [Create and remove room](./room.md)
- [Add and remove client from room](./client.md)
- [Signal negotiation](./signal.md)
- [Publishing simulcast video](./simulcast.md)
- [Publishing scalable video codec(SVC)](./svc.md)
- [Subscribe and view video](./video-subscription.md)
- [Send receive message through data channel](./data-channel.md)
- [Voice activity detection](./vad.md)
- [Statistics](./statistics.md)