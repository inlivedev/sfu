module github.com/samespace/sfu

go 1.21

// replace github.com/pion/transport => ../../pion/pion-transport
// replace github.com/pion/interceptor => ../../pion/pion-interceptor

require (
	github.com/pion/interceptor v0.1.29
	github.com/pion/rtcp v1.2.14
	github.com/stretchr/testify v1.9.0
)

require (
	github.com/jaevor/go-nanoid v1.4.0
	github.com/pion/ice/v3 v3.0.13
	github.com/pion/turn/v3 v3.0.3
	github.com/pion/webrtc/v4 v4.0.0-beta.26
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56
	golang.org/x/text v0.16.0
)

require (
	github.com/go-task/slim-sprig v0.0.0-20230315185526-52ccab3ef572 // indirect
	github.com/google/pprof v0.0.0-20210407192527-94a9f03dee38 // indirect
	github.com/onsi/ginkgo/v2 v2.9.5 // indirect
	github.com/pion/dtls/v3 v3.0.0 // indirect
	github.com/pion/mdns/v2 v2.0.7 // indirect
	github.com/pion/srtp/v3 v3.0.3 // indirect
	github.com/pion/stun/v2 v2.0.0 // indirect
	github.com/pion/transport/v3 v3.0.6 // indirect
	github.com/quic-go/quic-go v0.46.0 // indirect
	github.com/wlynxg/anet v0.0.3 // indirect
	go.uber.org/mock v0.4.0 // indirect
	golang.org/x/mod v0.19.0 // indirect
	golang.org/x/sync v0.7.0 // indirect
	golang.org/x/tools v0.23.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/uuid v1.6.0
	github.com/pion/datachannel v1.5.8 // indirect
	github.com/pion/dtls/v2 v2.2.12 // indirect
	github.com/pion/logging v0.2.2
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtp v1.8.7
	github.com/pion/sctp v1.8.19 // indirect
	github.com/pion/sdp/v3 v3.0.9
	github.com/pion/transport/v2 v2.2.9 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.25.0 // indirect
	golang.org/x/net v0.27.0 // direct
	golang.org/x/sys v0.22.0
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
