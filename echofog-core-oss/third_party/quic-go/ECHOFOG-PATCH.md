# Local fork of apernet/quic-go

This is `github.com/apernet/quic-go` at
`v0.63.1-0.20261004180939-a10df75c260c`, the version EchoFog already depended on,
with four numbered patches (§1–§4) and two earlier local changes that predate the
numbering (§5). `go.mod` in `echofog-core-oss` points at this copy with a `replace`
directive. Test files (`*_test.go`), mocks, and the `integrationtests/`, `interop/`,
`example/`, `testutils/` and `metrics/` trees were dropped to keep the copy reviewable;
nothing in the library's own build path imported them. The exceptions are the fuzz
target for patch 3 and the tests of patch 4; see below.

To review, diff this tree against the upstream module at that version
(`go mod download github.com/apernet/quic-go@v0.63.1-0.20261004180939-a10df75c260c`
gives the cache path). Nine files differ in total: §1–§4 explain seven of them, §5 the
other two (`chrome_parrot.go`, `datagram_queue.go`).

## 1. Real GREASE in the Chrome QUIC ClientHello
`internal/handshake/chrome_client_hello.go`

The `chromeQUICClientHelloSpec` deliberately omitted GREASE (a comment there said
so). Real Chrome QUIC sends GREASE, and a JA3/JA4 censor flags its absence as "a
library, not a browser". Added, all via uTLS's `GREASE_PLACEHOLDER` so uTLS fills
each with a per-connection `0x?a?a` value from its GREASE seed:

- a GREASE cipher suite, first in the list;
- two GREASE-typed extensions bracketing the extension list (first and last —
  `ShuffleChromeTLSExtensions` treats GREASE as positionally invariant, as Chrome
  does);
- a GREASE curve in supported_groups, a GREASE version in supported_versions, and
  a GREASE key share (one zero byte of key data), each first in its list.

The GREASE values are drawn from uTLS's GREASE seed, which is read *after* the
32-byte ClientHello random, so this does not disturb EchoFog's stealth token
(carried as that random via `tls.Config.Rand`).

## 2. Expose the TLS exporter from the uTLS (ChromeParrot) handshake
`internal/handshake/tls_conn_utls.go`, `internal/handshake/crypto_setup.go`,
`internal/handshake/interface.go`, `connection.go`

`utlsQUICConn.ConnectionState()` rebuilds a `crypto/tls.ConnectionState` field by
field. Its exporter (`ekm`) is an unexported func that cannot be carried across
from uTLS, so `ConnectionState().TLS.ExportKeyingMaterial` panics on a nil `ekm`.
That breaks EchoFog's peer-auth channel binding, which needs the TLS exporter.

uTLS's own `ConnectionState` keeps a working exporter (the handshake is TLS 1.3
and the config leaves `Renegotiation` at `RenegotiateNever`). Added a path that
reaches it:

- `utlsQUICConn.ExportKeyingMaterial` → the uTLS conn's exporter;
- `cryptoSetup.ExportKeyingMaterial` → the uTLS one for the parrot, else the
  crypto/tls `ConnectionState` exporter (which already carries `ekm`);
- the method on the `CryptoSetup` and `cryptoStreamHandler` interfaces;
- `Conn.ExportKeyingMaterial`, the public entry EchoFog's `quicMuxConn`
  calls instead of `ConnectionState().TLS.ExportKeyingMaterial`.

No existing behaviour changes: the stdlib (non-parrot) path returns exactly what
`ConnectionState().TLS.ExportKeyingMaterial` did.

## 3. The negotiated TLS group on the uTLS path, and the QUIC payload ceiling
`internal/handshake/tls_conn_utls.go`, `internal/handshake/server_hello_group.go`,
`interface.go`, `config.go`, `connection.go`

EchoFog decides sealed mode from the negotiated TLS group (`docs/protocol/v2/wire.md`
§8.5): `X25519MLKEM768` is never sealed, a classical group is. On the stdlib path
`crypto/tls` fills `ConnectionState.CurveID`. The ChromeParrot adapter rebuilt
`ConnectionState` field by field and left `CurveID` at zero, although uTLS knows
the group (it keeps it in the unexported `testingOnlyCurveID`). A uTLS client
therefore saw a classical group where a `crypto/tls` server saw
`X25519MLKEM768`: the two sides disagreed about sealing and the path could not
carry a stream.

The adapter now reads the group from the ServerHello it is already handed. The
Initial-level handshake bytes are buffered across CRYPTO frames until one complete
handshake message is available; `serverHelloKeyShareGroup` parses the
`key_share` entry of a ServerHello and skips a HelloRetryRequest (both are
`server_hello`, distinguished by the fixed HelloRetryRequest random), so the
second ServerHello decides. `ConnectionState()` then sets
`CurveID: tls.CurveID(c.curveID)`. A group that cannot be read stays zero, which
EchoFog treats as classical (sealed), never as post-quantum.

The same patch makes the advertised `max_udp_payload_size` configurable. It usedto be hardcoded to 1452 in the transport parameters of both client and server.
`wire.md` §5.1 requires the noise3 value to be reduced by the L0 packet overhead
and the shape profile's maximum padding, because the inner QUIC datagram must
still fit one outer datagram. `quic.Config` gained `MaxUDPPayloadSize` (uint16;
0 keeps the old 1452; below 1200 is rejected). Path MTU discovery already treats
the peer's advertised value as its ceiling, so both ends of a noise3 path settle
on the reduced size. `ChromeParrot` still overwrites the parameter with Chrome's
1472, since there the value is part of the fingerprint.

`internal/handshake/server_hello_group_test.go` is the only test file in this
copy: a fuzz target for the parser above (30 s clean, 3.6M execs).

## 4. A hook for the EchoFog REALITY v2 token in `ClientHello.random`
`interface.go`, `config.go`, `connection.go`, `internal/handshake/crypto_setup.go`,
`internal/handshake/tls_conn_utls.go`, `internal/handshake/client_hello_hook.go`,
`internal/handshake/chrome_client_hello.go`

`quic.Config.ClientHelloHook` is called before a `ChromeParrot` client builds its
ClientHello. It receives two things and returns the 32 bytes to put in
`ClientHello.random`:

- `ch0` — the exact marshalled ClientHello handshake message (type byte, 3-byte
  length, body) with `random` zeroed. This is the value `wire.md` §3.2 hashes into
  the token's associated data.
- `c` — the private X25519 key of the hello's `X25519` key share (or of the
  `X25519MLKEM768` share's X25519 half when there is no separate `X25519` entry).
  The token key is derived from `X25519(c, mask_pub)`, so the client must know it.

The token cannot be computed in one pass: `random` is drawn before the key shares
exist, and the associated data covers the finished hello. Patch 4 therefore builds
the hello twice over one recorded randomness stream:

1. **Pre-pass.** A throwaway uTLS QUIC client builds the hello with `crypto/rand`
   (or the caller's `tls.Config.Rand`) behind a recording reader. The reader serves
   zeros for the first 32 bytes — the client random — and records everything else.
   The resulting `QUICWriteData` event at the Initial level is CH0.
2. **(T12.6) The fork picks the X25519 key itself.** uTLS generates key shares with
   `ecdh.GenerateKey(rand)`, which since Go 1.24 ignores `rand` unless
   `GODEBUG=cryptocustomrand=1`, so the key cannot be learned from the randomness stream. The
   fork instead creates one X25519 key (`NewPrivateKey` over 32 `crypto/rand` bytes) and one
   ML-KEM-768 key, writes their public halves into the X25519 and X25519MLKEM768 shares of the
   spec (uTLS keeps a share whose `Data` is already set; as in Chrome one X25519 key serves both
   shares), and after `ApplyPreset` installs the private keys in
   `UConn.HandshakeState.State13.KeyShareKeys`. `UQUICConn` hides its `*UConn`, so
   `uconnOf` (`internal/handshake/utls_uconn.go`) reaches it with `reflect` + `unsafe` and fails
   closed if the uTLS layout changes (test `TestUconnOf`). `checkShare` verifies the key against
   CH0's share before the hook runs.
3. The hook returns the token, and the real handshake replays the recorded stream
   with the token in place of the first 32 bytes. Every other byte of the hello is
   identical, so the server's `CH0` equals the one the hook saw.

Two parts of the hello that uTLS draws from `crypto/rand` rather than from
`tls.Config.Rand` are shared between the passes: the GREASE ECH extension (its
payload length and contents) and the `quic_transport_parameters` extension (its
greased transport parameter). The extension order, which Chrome permutes per
connection, is drawn once per connection from `crypto/rand` and applied to both
passes with the same seed.

**No toolchain requirement (since T12.6).** The hook no longer depends on `crypto/ecdh`
honouring `tls.Config.Rand`, so neither `GODEBUG=cryptocustomrand=1` nor `//go:debug` is needed
and the client works inside a gomobile library, which has no `main` package. The recording and
replay readers still serve the one-byte `MaybeReadByte` probe from a separate source in case the
setting is on for other reasons.

`client_hello_hook_test.go` checks the whole path: the hook's `CH0`, the private key
it receives against the hello's share, the random on the wire (decrypted from the
client's Initial packets) and a completed handshake. It runs without any GODEBUG.


## 4a. `InitialPacketSize` below Chrome's under ChromeParrot (T3.12)

`config.go`: ChromeParrot pins `initialPacketSize` to Chrome's 1250. A smaller explicit
`Config.InitialPacketSize` is now kept (never a larger one). The REALITY v2 client asks for 1232
towards an IPv6 address, so the Initial datagram (1232 + 48) fits a path of PMTU 1280.

## 5. Earlier local changes (present before patches 1-4)

These two changes were already in EchoFog's copy when the numbered patches were
introduced. They are listed here so the fork is fully accounted for; a diff against
upstream shows them alongside §1-§4.

### 5.1 `chrome_parrot.go`: `ChromeParrotKeyShare`
Adds the exported `ChromeParrotKeyShare(rand io.Reader, alpn []string) ([]byte, error)`,
a thin wrapper over `internal/handshake.ChromeParrotKeyShare`. It builds a throwaway
Chrome ClientHello over the given randomness and returns its key_share extension body.
v1's REALITY QUIC dial (`internal/network/mimicry`, `internal/network/quic_reality_conn.go`)
uses it to predict the key share before sending the stealth token. v2 does not use it;
the change is inert for callers that do not call it.

### 5.2 `datagram_queue.go`: deeper queues and a drop counter
Raises `maxDatagramSendQueueLen` from 32 to 64 and `maxDatagramRcvQueueLen` from 128 to
2048, so a burst (a video keyframe is hundreds of datagrams) is not discarded while the
reader pauses. Adds `DiscardedDatagrams() uint64`, a process-wide counter of received
DATAGRAM frames dropped because the receive queue was full. No wire behaviour changes;
buffers are bounded, and the queues live only for a connection.
