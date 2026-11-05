// Package reality2 is the REALITY v2 carrier of EchoFog v2: the `reality2-quic` suite of
// wire.md §3 and, from T2.8, the shared token code of `reality2-tcp` and `ws2` (wire.md §4).
//
// The responder sits on UDP next to a real HTTP/3 website (the cover). A client that holds the
// responder's masking key puts a 32-byte token into ClientHello.random and gets a QUIC
// connection of its own; every other packet is forwarded to the cover, which answers it. The
// node itself never answers a stranger (V1): it sends no Version Negotiation, no stateless reset
// and no Retry before the token is checked, and it never opens a cover connection in the
// stranger's direction.
//
// # Parts
//
//   - token.go: the client and server halves of wire.md §3.2 — k_tok, the ClientHello binding,
//     the 32-byte token - and the ClientHello parser that extracts C.
//   - quicinitial.go: decryption of QUIC v1 Initial packets and reassembly of the ClientHello
//     from CRYPTO frames, so the gate can see the token before quic-go does.
//   - gate.go: the net.PacketConn filter of wire.md §3.3. It admits a 4-tuple only after a
//     valid token and forwards everything else to the cover.
//   - cover.go: the outbound sockets that carry stranger packets to the cover (wire.md §3.3).
//   - replay.go: the 900-second token replay cache, a Bloom filter persisted through carrier.KV.
//   - client.go / server.go: carrier.Dialer and carrier.Listener on top of quicx.
//   - tcpclient.go / tcpserver.go / tcpconn.go: reality2-tcp (wire.md §4.1-§4.3): the uTLS Chrome
//     client with the token in legacy_session_id, the server that reads the first TLS record,
//     admits a valid token and proxies everything else to the cover, and the byte-stream mux
//     (mux.NewConn) over the TLS application data with profile padding.
//
// # The ClientHello hook
//
// The token has to be inside ClientHello.random before the TLS stack hashes the hello, but the
// binding of wire.md §3.2 covers CH0 — the same hello with a zeroed random. The fork's patch #4
// (third_party/quic-go/ECHOFOG-PATCH.md) resolves that circularity: it builds CH0 in a throwaway
// pass, hands it to a hook together with the private X25519 key of the hello's X25519 share, and
// installs the hook's 32 bytes as the random of the real handshake. The client never sets
// tls.Config.Rand itself, so the ephemeral key of the token is the key the live handshake uses.
//
// # Ephemeral key (T12.6)
//
// The token is derived from the private key of the hello's X25519 key share. The quic-go fork
// (patch 4) picks that key itself and hands it to the hook, so the client does not depend on
// crypto/ecdh honouring tls.Config.Rand and needs no //go:debug cryptocustomrand=1 or GODEBUG:
// a gomobile library has no main package to carry the directive.
//
// # Manual cover until T2.6b
//
// Until the cover profile is measured (T2.6b), the listener takes the group and certificate of
// the cover from its configuration (CoverProfile) and refuses to start without them: it cannot
// reproduce an unknown cover, and serving with the wrong one is what a censor recognises.
package reality2
