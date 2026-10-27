# Network Reliability (NACK, RED, FEC)

Real-world networks are rarely perfect. Packet loss, jitter, and congestion can degrade video calls. inLive SFU uses several mechanisms to combat these issues.

## Error Correction Mechanisms

### 1. NACK (Negative Acknowledgement)
The most efficient way to handle packet loss. When the receiver detects a missing RTP packet, it sends a NACK feedback message (via RTCP) to the sender, asking for a retransmission.

*   **Best for**: Moderate packet loss and low-latency environments.
*   **SFU Support**: Enabled by default for all video tracks. The SFU caches recent packets and re-sends them upon request. Retransmissions are rate limited to the congestion controller's target bitrate, so a burst of NACKs on a congested link does not add to the congestion.

### 2. RED (Redundancy Encoding)
A method specifically used for audio (Opus). The sender includes a copy of the *previous* audio packet inside the *current* packet. If one packet is lost, the receiver can still recover the audio from the next one.

*   **Best for**: Critical audio tracks (like speakers) in high-packet-loss environments (WiFi/Cellular).
*   **SFU Support**: Support for `audio/red`.
*   **Technical Implementation**: See the client-side example in the **[Publishing Media](./publishing-media.md)** guide for how to prioritize RED in your codec preferences.

### 3. FEC (Forward Error Correction)
A proactive approach where the sender adds extra "recovery" packets created by XORing multiple data packets. If a packet is lost, the receiver uses the recovery packet to mathematically reconstruct the missing data.

*   **Best for**: Extreme packet loss where NACK retransmissions would take too long.
*   **SFU Support**: The SFU can handle and forward FlexFEC and other FEC formats depending on the client's capabilities.

## Monitoring Reliability

You can monitor the health of a client's connection using the `client.Stats()` method. Look for high `PacketLoss` percentages to determine if error correction is working effectively.

```go
stats := client.Stats()
if stats.PacketLoss > 5.0 {
    // Over 5% packet loss, user experience may be degraded
}
```

## Practical Tips for Developers

1.  **Always use NACK**: It is the industry standard for low-latency WebRTC and has the lowest overhead.
2.  **Enable RED for Audio**: If your users are frequently on mobile devices or poor WiFi, enabling `audio/red` is the single best thing you can do for audio clarity.
3.  **Adaptive Bitrate**: Ensure your `RoomOptions` have appropriate bitrate configurations. When the SFU detects congestion, lowering the bitrate is often more effective than error correction alone.
