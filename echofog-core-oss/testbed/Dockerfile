# Image of the test bed: the test node and the tools that make a Linux container behave like a
# router (NAT, latency, loss) or a victim of one (iptables, tc).
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# third_party holds the local apernet/quic-go fork that go.mod replaces to; go mod download must see
# its go.mod to build the module graph, so it has to be present before the dependency-cache step.
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /testnode ./testbed/testnode

FROM alpine:3.21
RUN apk add --no-cache iproute2 iproute2-tc iptables curl ca-certificates tzdata tcpdump
COPY --from=build /testnode /usr/local/bin/testnode
COPY testbed/entrypoint.sh /usr/local/bin/entrypoint.sh
# a Windows checkout may have turned the line endings into CRLF, which makes the shebang fail
RUN tr -d '\r' < /usr/local/bin/entrypoint.sh > /tmp/entrypoint.sh && mv /tmp/entrypoint.sh /usr/local/bin/entrypoint.sh && chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
