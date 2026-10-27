BINARY  := bin/echofog-node
VERSION ?= dev

.PHONY: all build test test-race test-calls test-calls-4k test-calls-tcp test-tun vet lint vuln fuzz testbed testbed-chaos clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/echofog-node

test:
	go test ./... -count=1

vet:
	go vet ./...

# Static analysis, known vulnerabilities, fuzzing (FUZZTIME seconds per target) and the Docker test bed
lint:
	go run honnef.co/go/tools/cmd/staticcheck@latest -checks "all,-ST1000,-ST1003,-ST1005,-ST1016,-ST1020,-ST1021,-ST1022,-ST1023" ./...

vuln:
	go run ./scripts/vulncheck

FUZZTIME ?= 30
fuzz:
	scripts/fuzz.sh $(FUZZTIME)

testbed:
	go run ./testbed/runner -scenario smoke

testbed-chaos:
	go run ./testbed/runner -scenario chaos

# The race detector needs cgo and a Linux toolchain; run it in Docker when the host cannot.
test-race:
	docker run --rm -v "$(CURDIR)":/src -w /src -e GOFLAGS=-buildvcs=false golang:1.27.3 sh -c 'go test -race -count=1 $$(go list ./... | grep -v -e /internal/simulation -e /testbed)'

# Call trees: unit tests and the 10-node QUIC cluster
test-calls:
	go test ./internal/services/call/... -count=1
	go test ./internal/simulation/ -run TestCluster_Calls -count=1 -v

# The same cluster streaming 4K60 and 1440p60
test-calls-4k: export SIM_CALLS_QUALITY = 2160p60,1440p60
test-calls-4k:
	go test ./internal/simulation/ -run TestCluster_Calls -count=1 -v

# The same call with UDP unavailable: every link over TCP/TLS mimicry + smux
test-calls-tcp: export SIM_TRANSPORT = tcp
test-calls-tcp:
	go test ./internal/simulation/ -run TestCluster_Calls -count=1 -v

# Whole-computer mode on a real Linux TUN (needs root and /dev/net/tun), inside Docker
test-tun:
	docker run --rm --cap-add NET_ADMIN --device /dev/net/tun --add-host echo.test:127.0.0.1 \
		-v "$(CURDIR)":/src -w /src -e GOFLAGS=-buildvcs=false golang:1.27.3 \
		sh -c "apt-get update -qq && apt-get install -y -qq iproute2 >/dev/null && go test -tags tune2e -run TestLinuxTunEndToEnd -count=1 -v ./internal/network/tunmode/"

clean:
	rm -rf bin
