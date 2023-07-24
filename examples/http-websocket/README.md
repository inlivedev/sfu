# inLive Hub HTTP Websocket Example

This example shows how to use the HTTP Websocket as a signaling protocol for the inLive Hub. This example contains a static HTTP server to serve the basic HTML file as our WebRTC client. The client will connect through the HTTP WebSocket to exchange the SDP and ICE candidates.

This example is using a single room only. To support multiroom, you need to create an endpoint to create a new room and store the session on a map variable.

To run this example, follow the steps below:
1. Run the HTTP server:
```bash
go run main.go
```
1. Open the [browser](http://localhost:8080) to open the client.
2. Click the `Start` button to start the WebRTC connection.
3. Open another [browser tab](http://localhost:8080) to start a different client.
4. Click the `Start` button to start the WebRTC connection, you should see the video stream from the first client.
5. Repeat the steps above to add more clients.