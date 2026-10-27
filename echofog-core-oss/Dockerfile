# Multi-stage build of a headless EchoFog node: a static binary in a small non-root image.

FROM golang:1.27.3-alpine AS builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /echofog-node ./cmd/echofog-node

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata \
 && addgroup -g 1000 echofog \
 && adduser -u 1000 -G echofog -s /bin/sh -D echofog \
 && mkdir -p /data && chown echofog:echofog /data
COPY --from=builder /echofog-node /usr/local/bin/echofog-node

USER echofog
VOLUME ["/data"]

# 4242/udp  QUIC with obfuscation: messages, calls, proxy traffic
# 4242/tcp  TCP fallback (TLS mimicry); publish 443 as well when using --tcp-listen :443
EXPOSE 4242/udp 4242/tcp

ENV ECHOFOG_DATA_DIR=/data \
    ECHOFOG_LISTEN_ADDR=:4242

# healthy once the node answers on its local status endpoint
HEALTHCHECK --interval=30s --timeout=6s --start-period=40s --retries=3 \
  CMD ["/usr/local/bin/echofog-node", "status", "--quiet"]

# echofog-node init | restore | run | status | wallet | invite  (see: echofog-node help)
ENTRYPOINT ["/usr/local/bin/echofog-node"]
CMD ["run"]
