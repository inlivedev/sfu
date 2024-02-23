module github.com/inlivedev/sfu

go 1.21

// replace github.com/pion/transport => ../../pion/pion-transport
// replace github.com/pion/interceptor => ../../pion/pion-interceptor

require (
	github.com/pion/ice/v2 v2.3.11
	github.com/pion/interceptor v0.1.25
	github.com/pion/rtcp v1.2.12
	github.com/pion/webrtc/v3 v3.2.24
	github.com/stretchr/testify v1.8.4
)

require (
	github.com/golang/glog v1.1.2
	github.com/jaevor/go-nanoid v1.3.0
	github.com/pion/transport/v3 v3.0.1
	golang.org/x/exp v0.0.0-20230905200255-921286631fa9
	golang.org/x/text v0.14.0
)

require (
	github.com/felixge/fgprof v0.9.3 // indirect
	github.com/google/pprof v0.0.0-20211214055906-6f57359322fd // indirect
	github.com/pkg/profile v1.7.0 // indirect
	go.uber.org/goleak v1.3.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/uuid v1.3.1
	github.com/pion/datachannel v1.5.5 // indirect
	github.com/pion/dtls/v2 v2.2.7 // indirect
	github.com/pion/logging v0.2.2
	github.com/pion/mdns v0.0.8 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtp v1.8.3
	github.com/pion/sctp v1.8.8 // indirect
	github.com/pion/sdp/v3 v3.0.6
	github.com/pion/srtp/v2 v2.0.18 // indirect
	github.com/pion/stun v0.6.1 // indirect
	github.com/pion/transport/v2 v2.2.3 // indirect
	github.com/pion/turn/v2 v2.1.3
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.16.0 // indirect
	golang.org/x/net v0.19.0 // direct
	golang.org/x/sys v0.15.0
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
