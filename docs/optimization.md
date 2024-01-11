# Improving Video Call Quality
When you experiences a video glitch or the audio is not clear in your video call, most likely it caused by the packet loss. The packet loss most of the time is caused by the network congestion. The network congestion can be caused by the following reasons:
- The network bandwidth is not enough when the sending bitrate is higher than the bandwidth.
- The network is not reliable (e.g. WiFi interference) that cause the some packet just loss in the network.

Handling the bandwidth issue is not that hard because the WebRTC already come with the bandwidth estimation algorithm, and adaptive bitrate mechanism like simulcast or SVC. But to handle the reliability issue, we need to do some extra work. There are some common ways to handle the reliability issue:
- NACK - Negative Acknowledgement
- RED - Redundancy Encoding
- FEC - Forward Error Correction



## NACK
NACK is a mechanism that the receiver will send a NACK packet to the sender when it detects a packet loss. The sender will retransmit the lost packet when it receives the NACK packet. The NACK packet is sent via RTCP protocol. 

## RED
RED is a mechanism that the sender will send some extra redundant packets to the receiver. The receiver can use the redundant packets to recover the lost packets. The redundant packets are sent via RTP protocol.

## FEC
FEC is more advance mechanism of redundancy method. FEC add more packets that can be used to restore other packets that are lost. The most common approach for FEC is by taking multiple packets, XORing them and sending the XORed result as an additional packet of data. If one of the packets is lost, we can use the XORed packet to recreate the lost one.


## When to use NACK, RED or FEC?
- Always use NACK if the network is reliable. NACK is more efficient than RED and FEC because it only send the lost packet when it is needed.
- Use RED or FEC if the network is not reliable, or you want to guarantee some media tracks(audio or speakers track) realibility in conference call. 


## Advance use of RED and FEC in SFU
RED and FEC can be used to improve the video quality in the video call. The idea is to send the redundant packets with lower quality than the original packets. The receiver can use the redundant packets to improve the video quality. But it will required more bandwidth to send and receive the redundant packets. Some of ideas to optimize the use of RED and FEC are:
- We need to able detect the sender bandwidth if it has enough bandwidth to send the redundant packets.
- Then we need to detect the receiver network realibility if it will need the redundant packets to improve the video quality. If the receiver network is reliable, we can disable the redundant packets to save the bandwidth.
- SFU can be use to decide which receiver need the redundant packets. The SFU can detect the receiver network realibility and send the redundant packets to the receiver that need it.
- We can let the sender send a normal packet without RED/FEC, but then we can re-encode the packet in the SFU and send the redundant packets to the receiver that need it. This approach will save the bandwidth in the sender side, but it will required more CPU usage in the SFU side.



