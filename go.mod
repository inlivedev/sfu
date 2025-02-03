module github.com/inlivedev/sfu

go 1.23

// replace github.com/pion/transport => ../../pion/pion-transport
// replace github.com/pion/interceptor => ../../pion/pion-interceptor

require (
	github.com/pion/interceptor v0.1.37
	github.com/pion/rtcp v1.2.15
	github.com/stretchr/testify v1.10.0
)

require (
	github.com/jaevor/go-nanoid v1.3.0
	github.com/pion/ice/v4 v4.0.5
	github.com/pion/turn/v4 v4.0.0
	github.com/pion/webrtc/v4 v4.0.8
	golang.org/x/exp v0.0.0-20230905200255-921286631fa9
	golang.org/x/text v0.21.0
)

require (
	github.com/pion/dtls/v3 v3.0.4 // indirect
	github.com/pion/mdns/v2 v2.0.7 // indirect
	github.com/pion/srtp/v3 v3.0.4 // indirect
	github.com/pion/stun/v3 v3.0.0 // indirect
	github.com/pion/transport/v3 v3.0.7 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/uuid v1.6.0
	github.com/pion/datachannel v1.5.10 // indirect
	github.com/pion/logging v0.2.2
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtp v1.8.11
	github.com/pion/sctp v1.8.35 // indirect
	github.com/pion/sdp/v3 v3.0.10
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/net v0.33.0 // direct
	golang.org/x/sys v0.28.0
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
