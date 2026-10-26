# Local fork of apernet/quic-go

This is `github.com/apernet/quic-go` at
`v0.63.1-0.20261004180939-a10df75c260c`, the version EchoFog already depended on,
with two small, self-contained changes. `go.mod` in `echofog-core-oss` points at
this copy with a `replace` directive. Test files (`*_test.go`), mocks, and the
`integrationtests/`, `interop/`, `example/`, `testutils/` and `metrics/` trees
were dropped to keep the copy reviewable; nothing in the library's own build path
imported them.

To review, diff this tree against the upstream module at that version
(`go mod download github.com/apernet/quic-go@v0.63.1-0.20261004180939-a10df75c260c`
gives the cache path). The only functional changes are the two below.

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
