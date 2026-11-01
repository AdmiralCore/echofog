# Third-party notices

EchoFog includes and adapts code from the projects below. Their licenses require that these
notices travel with the code. Every adapted file also names its origin in a header comment.

## go-libp2p (MIT License)

Source: https://github.com/libp2p/go-libp2p

Used in:

- `internal/network/portmap/portmap.go` and the tests beside it, from `p2p/net/nat`: renewal of
  router port mappings, external-address caching and re-discovery after a router restart.
- `internal/p2p/holepunch/holepunch.go`, from `p2p/protocol/holepunch`: the DCUtR coordination
  (CONNECT, SYNC, round-trip-time based start, three attempts with a role swap).
- `internal/network/holepunch_quic.go`, from `p2p/transport/quic/transport.go`: the random-packet
  pacing of a QUIC hole punch.

```
The MIT License (MIT)

Copyright (c) 2014 Juan Batiz-Benet

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```

## go-libp2p `p2p/net/nat/internal/nat`, forked from fd/go-nat (Apache License 2.0)

Source: https://github.com/libp2p/go-libp2p (originally https://github.com/fd/go-nat)

Used in `internal/network/portmap/gateway/` (UPnP and NAT-PMP discovery and port mapping). The
full license text is in `internal/network/portmap/gateway/LICENSE`. Changes: logging goes through
`log/slog` and the package is renamed. `pcp.go` in the same folder (PCP, RFC 6887) is EchoFog's own
code.

## hysteria (MIT License)

Source: https://github.com/apernet/hysteria

Used in `internal/network/congestion/bbr/`, `internal/network/congestion/brutal/` and
`internal/network/congestion/common/`: the BBR and Brutal congestion controllers and the pacer.
`internal/network/congestion/congestion.go` names the origin. The full license text is in
`internal/network/congestion/LICENSE-hysteria.md` (Copyright 2023 Toby).

## apernet/quic-go, a fork of quic-go (MIT License)

Source: https://github.com/apernet/quic-go (forked from https://github.com/quic-go/quic-go)

A modified copy is in `third_party/quic-go/`, and `go.mod` points at it with a `replace`
directive. The changes (Chrome GREASE in the QUIC ClientHello, the TLS exporter of the uTLS
handshake) are listed in `third_party/quic-go/ECHOFOG-PATCH.md`. The full license text is in
`third_party/quic-go/LICENSE` (Copyright (c) 2016 the quic-go authors & Google, Inc.).

## Tailscale (BSD 3-Clause License)

Source: https://github.com/tailscale/tailscale, `net/netcheck`

The rule used by `internal/network/natcheck` to tell a hole-punchable NAT from an unpunchable one
(compare the public addresses reported by several independent observers; if they differ, the
mapping depends on the destination) follows Tailscale's `MappingVariesByDestIP`. The implementation
here is our own; the idea and the rule are theirs.

```
BSD 3-Clause License

Copyright (c) 2020 Tailscale Inc & contributors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its
   contributors may be used to endorse or promote products derived from
   this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
