# The v2 server node: a static binary in a small image that runs as an unprivileged user.
# Build context is echofog-core-oss:   docker build -f deploy/v2/Dockerfile -t echofog-node:v2-local .
# The entrypoint creates the profile on the first start (deploy/v2/entrypoint.sh), then runs
# "echofog-node v2 run". Nothing secret is built into a layer: keys are made at run time in /data.

# The builder runs on the build machine's own platform and cross-compiles, so a multi-platform build
# does not compile Go under emulation. The Go version is the one in go.mod (a test keeps them equal).
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder
ARG TARGETOS TARGETARCH
RUN apk add --no-cache git ca-certificates
WORKDIR /build
COPY go.mod go.sum ./
# third_party holds the local quic-go fork that go.mod replaces to; go mod download must see it.
COPY third_party ./third_party
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /echofog-node ./cmd/echofog-node

FROM alpine:3.21
LABEL org.opencontainers.image.title="echofog-node" \
      org.opencontainers.image.description="EchoFog v2 server node" \
      org.opencontainers.image.source="https://github.com/AdmiralCore/echofog"
RUN apk --no-cache add ca-certificates tzdata \
 && addgroup -g 1000 echofog \
 && adduser -u 1000 -G echofog -s /sbin/nologin -D echofog \
 && mkdir -p /data && chown echofog:echofog /data && chmod 700 /data
COPY --from=builder /echofog-node /usr/local/bin/echofog-node
COPY --chmod=0555 deploy/v2/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1000:1000
VOLUME ["/data"]

# The node serves noise3 over UDP only. ECHOFOG_PORT picks the port (default 443); invitations,
# when switched on, use one more UDP port that is not published by default.
EXPOSE 443/udp

ENV ECHOFOG_DATA_DIR=/data

# Healthy once the operator API on loopback answers: the node is up and the token works.
HEALTHCHECK --interval=30s --timeout=6s --start-period=40s --retries=3 \
  CMD ["/usr/local/bin/echofog-node", "v2", "status", "--data", "/data"]

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["v2", "run"]
