# Builds nDPI from source and the EchoFog traffic generator, then captures loopback while the
# generator runs and classifies the capture with ndpiReader.
FROM golang:1.27.3 AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
# third_party holds the local apernet/quic-go fork that go.mod replaces to; go mod download must see
# its go.mod to build the module graph, so it has to be present before the dependency-cache step.
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/ndpigen ./testbed/ndpigen

FROM debian:bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
      git build-essential autoconf automake libtool pkg-config \
      libpcap-dev libgcrypt20-dev tcpdump ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# a pinned recent nDPI release
RUN git clone --depth 1 --branch 4.14 https://github.com/ntop/nDPI.git /nDPI \
    && cd /nDPI && ./autogen.sh && make -j"$(nproc)" \
    && cp example/ndpiReader /usr/local/bin/ndpiReader
COPY --from=gobuild /out/ndpigen /usr/local/bin/ndpigen
COPY testbed/ndpigen/run.sh /run.sh
RUN chmod +x /run.sh
ENTRYPOINT ["/run.sh"]
