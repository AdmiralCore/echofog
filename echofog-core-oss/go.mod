module github.com/echofog/echofog-core

go 1.27.3

require (
	filippo.io/edwards25519 v1.2.0
	github.com/apernet/quic-go v0.63.1-0.20261004180939-a10df75c260c
	github.com/awnumar/memguard v0.23.0
	github.com/dgraph-io/badger/v4 v4.9.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/grandcat/zeroconf v1.0.0
	github.com/huin/goupnp v1.3.0
	github.com/jackpal/go-nat-pmp v1.0.2
	github.com/klauspost/compress v1.18.0
	github.com/koron/go-ssdp v0.9.1
	github.com/libp2p/go-netroute v0.4.0
	github.com/pion/interceptor v0.1.29
	github.com/pion/rtcp v1.2.14
	github.com/pion/rtp v1.8.7
	github.com/pion/sdp/v3 v3.0.9
	github.com/pion/stun v0.6.1
	github.com/pion/webrtc/v3 v3.3.6
	github.com/refraction-networking/utls v1.8.2
	github.com/stretchr/testify v1.12.1
	github.com/tyler-smith/go-bip39 v1.1.0
	github.com/xjasonlyu/tun2socks/v2 v2.6.0
	github.com/xssnick/tonutils-go v1.18.0
	github.com/xtaci/smux v1.5.57
	go.uber.org/goleak v1.3.0
	go.uber.org/mock v0.6.0
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/ajg/form v1.5.1 // indirect
	github.com/andybalholm/brotli v1.1.0 // indirect
	github.com/awnumar/memcall v0.4.0 // indirect
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgraph-io/ristretto/v2 v2.2.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-chi/chi/v5 v5.3.2 // indirect
	github.com/go-chi/cors v1.2.1 // indirect
	github.com/go-chi/render v1.0.3 // indirect
	github.com/go-gost/relay v0.5.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/flatbuffers v25.2.10+incompatible // indirect
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	github.com/gorilla/schema v1.4.1 // indirect
	github.com/miekg/dns v1.1.55 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/pion/datachannel v1.5.8 // indirect
	github.com/pion/dtls/v2 v2.2.12 // indirect
	github.com/pion/ice/v2 v2.3.38 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns v0.0.12 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/sctp v1.8.19 // indirect
	github.com/pion/srtp/v2 v2.0.20 // indirect
	github.com/pion/transport/v2 v2.2.10 // indirect
	github.com/pion/transport/v3 v3.1.1 // indirect
	github.com/pion/turn/v2 v2.1.6 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.37.0 // indirect
	go.opentelemetry.io/otel/metric v1.37.0 // indirect
	go.opentelemetry.io/otel/trace v1.37.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.11.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	golang.zx2c4.com/wireguard v0.0.0-20250521234502-f333402bd9cb // indirect
	gvisor.dev/gvisor v0.0.0-20250523182742-eede7a881b20 // indirect
)

// Local fork of apernet/quic-go: adds real Chrome GREASE (a GREASE cipher suite, a GREASE-typed
// extension and a GREASE key share) to the QUIC-REALITY ClientHello, and exposes the uTLS TLS
// exporter so the ChromeParrot path keeps EchoFog's peer-auth channel binding. The copy under
// third_party/ is the upstream module at the version above with exactly those two changes, so it
// can be diffed against upstream. See third_party/quic-go/ECHOFOG-PATCH.md.
replace github.com/apernet/quic-go => ./third_party/quic-go
