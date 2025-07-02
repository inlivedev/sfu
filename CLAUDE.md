# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the inLive Hub SFU - a Selective Forwarding Unit (SFU) library for WebRTC based on Pion. It's designed for group video calls, screen sharing, and real-time communication applications.

## Development Commands

### Build
```bash
# Build the main package
go build github.com/inlivedev/sfu

# Build with module mode
go build -mod=readonly github.com/inlivedev/sfu

# Run examples
cd examples/http-websocket && go run main.go
```

### Test
```bash
# Run all tests
go test ./...

# Run specific test groups with timeout
go test -timeout 5m -run '^(TestLeaveRoom|TestRenegotiation)$' github.com/inlivedev/sfu -mod=readonly
go test -timeout 5m -run '^(TestRoomCreateAndClose|TestRoomJoinLeftEvent|TestRoomStats)$' github.com/inlivedev/sfu -mod=readonly
go test -timeout 5m -run '^(TestTracksSubscribe|TestSimulcastTrack|TestClientDataChannel)$' github.com/inlivedev/sfu -mod=readonly

# Note: Tests require system dependencies on Ubuntu/Debian:
# sudo apt update && sudo apt install libx264-dev libopus-dev
```

### Code Quality
```bash
# Format code
go fmt ./...

# Vet code
go vet ./...

# Clean dependencies
go mod tidy
```

## Architecture Overview

The SFU has a hierarchical architecture with clear separation of concerns:

```
Manager (manager.go)
  └── Room(s) (room.go)
        └── SFU (sfu.go)
              └── Client(s) (client.go)
                    ├── Tracks (track.go, clienttrack*.go)
                    ├── DataChannels (datachannel.go)
                    └── BitrateController (bitratecontroller.go)
```

### Core Components

1. **Manager**: Top-level orchestrator managing multiple rooms with extension support
2. **Room**: Container for WebRTC sessions, manages client lifecycle and statistics
3. **SFU**: Core forwarding unit handling track distribution and peer connections
4. **Client**: Individual peer connection with track publishing/subscribing capabilities

### Key Subsystems

1. **Track System**: 
   - Interface-based design (`ITrack`) supporting regular, simulcast, and SVC tracks
   - Track pools in `pkg/rtppool/` for efficient packet management
   - Different track types: `ClientTrack`, `ClientTrackSimulcast`, `ClientTrackSVC`, `RelayTrack`

2. **Packet Management** (`pkg/rtppool/`):
   - Object pools to reduce GC pressure
   - `PacketManager` for NACK handling with retainable packets
   - `BufferPool` for reusable payload buffers

3. **Quality Control**:
   - 11 quality levels from `QualityNone` to `QualityAudioRed`
   - Adaptive bitrate control based on network conditions
   - Supports both simulcast layer switching and SVC quality adaptation

4. **Interceptors** (`pkg/interceptors/`):
   - `playoutdelay`: Jitter buffer management
   - `voiceactivedetector`: VAD for audio streams
   - `simulcast`: Layer switching logic

### Extension Points

- `IManagerExtension`: Room-level operations (authentication, custom logic)
- `IExtension`: Client-level operations
- Custom interceptors via Pion's framework

### Important Patterns

1. **Negotiation**: Uses "perfect negotiation" pattern with offer/answer exchange
2. **Renegotiation**: Supports both SFU-initiated and client-initiated with conflict resolution
3. **Track Publishing**: Two-step process - tracks added then source type set (`media`/`screen`)
4. **Memory Management**: Extensive use of object pools for RTP packets to minimize allocations

### Configuration

- Logging: Uses Glog with flags `-stderrthreshold=warning` and `-logtostderr=true`
- Room timeout: Configurable auto-cleanup after empty period
- Codec support: Comprehensive set including VP8/9, H264/5, AV1, Opus with RTX/RED