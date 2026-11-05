// Package quicx is the QUIC carrier of EchoFog v2 (wire.md §5.1, §6.5, handshake.md §3.2,
// wire.md §8.5). It wraps one established quic-go connection into a mux.Carrier: QUIC streams
// become RawStreams, QUIC DATAGRAM frames become carrier datagrams, the TLS exporter supplies
// the channel binding, and the negotiated TLS group decides sealed mode.
//
// Two suites sit on this package:
//
//   - noise3 (wire.md §2, §5.1): the inner TLS is X25519MLKEM768 only. The server certificate
//     is self-signed ECDSA P-256 with an empty subject and a random serial by default, because
//     the pinned Chrome ClientHello does not offer Ed25519; a cover profile that does offer
//     Ed25519 gets an Ed25519 certificate (Noise3TLSConfigForProfile). A classical group is a
//     protocol failure, so the noise3 dialer and listener use WrapNoise3.
//   - reality2-quic (wire.md §3, §5.1): the inner TLS mirrors the cover's group. With a
//     classical cover group the path runs in sealed mode; Wrap reports that through Sealed.
//
// The package only wraps an already established connection: it does no TLS of its own and
// knows nothing about the L1 handshake or services. The inner TLS certificates are not
// verified (wire.md §5.1); authentication is the Noise handshake, bound to this TLS session by
// ChannelBinding.
//
// # Channel binding
//
// ChannelBinding uses the TLS exporter of handshake.md §3.2: label
// "EXPORTER-echofog/hs2/channel", empty context, 32 bytes. It goes through
// quic.Conn.ExportKeyingMaterial (fork patch #2), never through
// ConnectionState().TLS.ExportKeyingMaterial: on the ChromeParrot (uTLS) path the rebuilt
// crypto/tls.ConnectionState cannot carry the exporter and that call panics.
//
// # Sealed mode
//
// Sealed reports !X25519MLKEM768 of the negotiated group, read from
// quic.Conn.ConnectionState().TLS.CurveID. Both paths fill CurveID: crypto/tls natively and
// the ChromeParrot adapter through fork patch #3, which parses the ServerHello's key_share
// group. Before patch #3 the client saw CurveID 0 where the server saw X25519MLKEM768 and the
// two sides disagreed about sealing. An unknown group (0) is treated as classical, so a
// broken group reading fails towards sealing rather than towards plaintext.
//
// A classical group is only acceptable when the path is in sealed mode. noise3 never is, so
// WrapNoise3 closes the connection with CLOSE{protocol-violation} on both ends when it sees
// one. reality2-quic accepts it and reports Sealed.
//
// # Unidirectional streams
//
// mux.Carrier has no unidirectional streams (internal/v2/mux/carrier.go), although QUIC does.
// The noise3 QUIC config therefore advertises MaxIncomingUniStreams = -1, which quic-go turns
// into 0 ("none"; plain 0 would mean quic-go's default of 100). A peer that opens a
// unidirectional stream anyway is refused: the carrier drains them and cancels reads. This is
// a local policy, not a need for the peer to know: v2 never uses one-way streams over QUIC.
//
// The reality2-quic client parrots Chrome, whose transport parameters advertise 103
// unidirectional streams, so that config cannot set -1 without changing the fingerprint. The
// carrier still drains and cancels every one it receives.
//
// # Datagrams
//
// SendDatagram is best effort, as mux.Carrier requires: datagrams queue up to datagramQueue
// entries and are dropped beyond that. QUIC itself never retransmits DATAGRAM frames.
package quicx
