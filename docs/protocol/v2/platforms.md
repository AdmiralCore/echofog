# Platforms: mobile builds and capability profiles

**Decision D41.** The v2 core MUST build with `gomobile bind` for Android first and iOS later, and a
phone MUST be a full v2 peer. Everything that differs between platforms is local policy, selected by a
runtime **profile** and explicit **capability flags** that the app passes at start. Nothing in this
document changes a byte on the wire.

This is not a protocol layer. It constrains implementations so that every platform speaks the
protocol of the other documents.

## 1. Core build

### 1.1 Targets and rules

- `gomobile bind -target=android/arm64,android/amd64` and `-target=ios/arm64` MUST succeed for the
  protocol core (everything below the app: L0–L7, services, storage). android/amd64 covers emulators.
- **No cgo in EchoFog's own code.** Cryptography, QUIC, the mux, the DHT and storage are pure Go, and
  no EchoFog package may `import "C"` (today none does; it must stay that way). iOS builds link through
  gomobile with the external linker the Go toolchain requires: Go uses external (cgo) linking for
  every `ios` target even when no package uses cgo, so `CGO_ENABLED=0` is not possible there. Android
  builds work with `CGO_ENABLED=0`.
- **Build tags with portable fallbacks.** Every platform-specific file sits behind a build tag, and
  every such file has a fallback that compiles everywhere else and keeps the protocol working, possibly
  slower. Note that `GOOS=android` also satisfies the `linux` tag and `GOOS=ios` satisfies `darwin`, so
  a file meant only for desktop Linux needs `linux && !android`.
- **No subprocesses.** The core MUST NOT run `os/exec` commands on mobile (the app sandbox forbids
  them).
- **No ambient environment.** The app passes the data directory, the profile, the flags and any file
  descriptors; the core reads no fixed paths, no current directory, no environment variables.
- **A thin binding package** (for example `mobile/`) exposes only types gomobile can bind (strings,
  numbers, byte slices, simple interfaces for callbacks). It is the only package the app imports.
- **Network discovery comes from the app.** On Android 11+ (`targetSdk ≥ 30`), apps cannot read
  netlink route information, so Go's `net.Interfaces()` and `net.InterfaceAddrs()` fail. The app
  passes local addresses and network changes (from `ConnectivityManager`/`LinkProperties`; on iOS from
  `NWPathMonitor`) through a callback; the core never relies on those two calls on mobile.
- **Own sockets must bypass the VPN.** When the app runs a `VpnService`, every socket the core opens for
  peers MUST be passed to the app's `protect(fd)` callback before use (otherwise the core's traffic
  loops into its own tunnel).

### 1.2 Pieces that are not portable today, and their fallbacks

Found in the current `dev` code (`echofog-core-oss` and `echofog-desktop-oss`):

| Piece | Where today | Problem on mobile | Fallback |
|---|---|---|---|
| UDP GSO send (`UDP_SEGMENT`) | `internal/network/dpi/gso_linux.go` (`//go:build linux`), used by `obfs_oob.go` | compiles on Android (linux tag) but kernels and SELinux policy vary; iOS has none | probe once at start; on failure, or with flag `udp=restricted`, one `WriteTo` per datagram. iOS: never compiled |
| Batched receive (`recvmmsg`) | `internal/network/dpi/mux_read_linux.go` (`ipv4.Message`, batch 16) | may be refused on some Android kernels | `dpi/mux_read_other.go` path: one `ReadFrom` per datagram, selected at run time after a failed batch read |
| ECN and other control messages | `internal/network/dpi/oob_unix.go` (`linux \|\| darwin \|\| freebsd`, `x/sys/unix`) | builds for android and ios; cmsg support differs | `oob_other.go`: no control messages; ECN not set (wire-neutral) |
| Windows socket options | `internal/network/bind_windows.go`, `exit_gateway/udp_transient_windows.go` | Windows only | already tag-separated; nothing needed |
| TUN with Wintun and WFP helper | `internal/network/tunmode/helper_windows.go`, `wintun_embed_*.go`, `wintun_*.bin` | Windows only | Android: TUN from the `VpnService` file descriptor handed to the core (§2, flag `tun_fd`); tun2socks engine fed by `fd://`. iOS: the `NEPacketTunnelProvider` packet flow through a callback |
| TUN on Linux via the `ip` and `resolvectl` commands | `internal/network/tunmode/helper_linux.go` (`exec.Command`), `helper.go` (`windows \|\| linux`) | the `linux` tag also selects it on Android, where subprocesses and root are unavailable | tag it `linux && !android`; Android uses `helper_other.go` plus the file-descriptor path |
| memguard locked pages | `internal/crypto/keymanager/*` (`memguard.NewEnclave`, 33 call sites) | `mlock` is limited (Android `RLIMIT_MEMLOCK` is often 64 KiB) or unavailable; failures must not stop the node | **best-effort mode**: try to lock; if locking fails, keep secrets in ordinary memory, still sealed with memguard's enclave encryption and wiped after use; report "memory protection reduced" to the app. Platform keystores (Android Keystore, iOS Keychain) protect the at-rest key instead |
| OS secret protection (DPAPI) | `internal/protection/osprotect/dpapi_windows.go`; `osprotect_other.go` returns none | no DPAPI | Android Keystore / iOS Keychain through an app callback that wraps and unwraps the storage key |
| UPnP, NAT-PMP, PCP | `internal/network/portmap/gateway/{upnp,natpmp,pcp}.go`, gateway discovery via `go-netroute` and `net.Interfaces()` | netlink route access is blocked on Android 11+; mobile networks are CGNAT, so mapping rarely works and costs battery | off on mobile (flag `port_mapping=false`); the node relies on hole punching and relays |
| STUN client | `internal/network/natcheck/stun.go` (uses `net.Interfaces()`) | as above; and v2 forbids STUN on carrier sockets anyway (wire.md §7.2) | OBSERVED_ADDR from authenticated peers only (flag `stun=false`) |
| mDNS | `internal/services/discovery/m_dns.go` (zeroconf) | multicast needs a lock on Android; not used by v2 | not built into mobile cores |
| Online GeoIP lookup | `pkg/api/node/geoip.go` | forbidden by wire.md §1 everywhere | built-in offline database |
| BadgerDB | `pkg/node/node.go` (`MemTableSize` 8 MiB, `BaseTableSize` 8 MiB, `ValueLogFileSize` 16 MiB, default caches and memtable count) | default block and index caches and several memtables use tens to hundreds of MiB; mobile apps are killed for memory | mobile options from flag `mem_cap_mb`: 1–2 memtables of 4 MiB, block cache ≤ 8 MiB, index cache ≤ 4 MiB, value log 16 MiB, compaction with 1 worker; storage code must keep working with these values (its repair tools already take the table size as a parameter) |
| Windows screen capture (WGC) and Media Foundation encoder | `echofog-desktop-oss/internal/nativecap/*_windows.go` | Windows only; app layer, not core | Android: `MediaProjection` + `MediaCodec` in the app, encoded units passed to the core's call service through the binding; iOS: ReplayKit + VideoToolbox |
| WebRTC loopback bridge (pion) | `internal/services/call/bridge/bridge.go` | exists to connect a desktop WebView; heavy for mobile | optional behind a build tag; mobile apps feed and receive encoded media units directly through the binding |
| Windows-only file helpers | `internal/services/filexfer/{diskfree,motw}_windows.go` | Windows only | already tag-separated |

## 2. Capability profile and flags

The app starts the core with a **profile** and explicit **flags**. The profile sets defaults; each flag
overrides one behaviour. Every flag is local policy: the peer sees only legal v2 behaviour (timing,
which roles are offered, which addresses are listed), never a different wire format.

`platform = android | ios | desktop | server`

| Flag | Values (mobile default) | Behaviour | Why it stays wire-compatible |
|---|---|---|---|
| `low_power` | on/off (on when the OS reports battery saver) | idle keepalive `uniform(40, 55)` s instead of `uniform(12, 24)` s; DHT bucket refresh every `uniform(45, 75)` min instead of 10–15; lazy PEX only when below 4 peers; no idle padding; no background relaying | keepalive stays below the 60 s path timeout (wire.md §7.1); refresh and PEX timing are local (dht.md §5, services.md §3); `idle-padding` is a feature the node simply does not offer |
| `metered` | on/off (from the OS) | no files/2 prefetch, no relaying for others, DHT republishing only of the node's own records, call video capped by the app | only what the node offers changes; all v2 messages are optional to send |
| `roles` | subset of {`relay`, `exit`, `mailbox`, `dht-server`} (empty on mobile) | the node offers only these; card flags and `services` list only them | cards and service lists are how roles are announced (objects.md §3); a node without `dht-server` still issues DHT queries |
| `background` | `foreground`, `limited`, `suspended` | `limited`: keep one session to a guard or mailbox relay, close others, rely on mailbox/2 FETCH with `wait`; `suspended`: close sessions with GOAWAY; on resume, fetch from mailboxes | GOAWAY, FETCH and reconnects are normal v2 behaviour. A push or wake-up path through the mailbox relay (feature `wake-push`, reserved, registries.md §5) comes later |
| `tun_fd` | file descriptor or −1 | the core reads and writes IP packets on this descriptor (Android `VpnService.establish()`; iOS packet flow through a callback) instead of creating a TUN device | TUN is local; the exit sees ordinary exit/2 streams |
| `protect` | callback | every peer socket is protected before use, so the core's traffic bypasses its own VPN | local |
| `local_addrs` | callback | the app reports local addresses and network changes (replaces `net.Interfaces()`) | addresses still reach peers only through cards, OBSERVED_ADDR and hole punching |
| `port_mapping` | on/off (off) | UPnP, NAT-PMP and PCP disabled | port mapping is invisible to peers |
| `stun` | on/off (off) | no STUN from any socket; public address from OBSERVED_ADDR only | already required for carrier sockets (wire.md §7.2) |
| `mem_cap_mb` | integer (Android: 128, iOS extension: 40) | BadgerDB options of §1.2; DHT cache ≤ 200 records; routing table limited to 32 buckets' worth of cards in memory, the rest on disk; at most 32 sessions, 16 relay circuits | sizes of local caches and session counts are not on the wire; refusals use the normal REJECT/CLOSE codes |
| `udp` | `full`, `restricted`, `blocked` | `restricted`: no GSO/recvmmsg/ECN, smaller noise3 padding range; `blocked`: only TCP suites (`reality2-tcp`, `ws2`) and relays | suite choice from the card is already the initiator's (wire.md §1) |
| `secret_store` | callback | wraps and unwraps the at-rest key with the platform keystore | local (L6) |

A mobile node never sets `role = server` and never runs a public listener for strangers; it accepts
inbound sessions from peers that know its peer card (after hole punching or through relays).

## 3. What mobile never changes on the wire

A phone is a full v2 peer:

- the same L0 suites with the same formats (noise3, reality2-quic, reality2-tcp, ws2), the same L1
  handshake suites, the same frames, features, services and service versions;
- the same signed objects (it has an account key, a device key and a device list like any node);
- it may decline to offer roles, features and services, as any node may; it MUST NOT shorten,
  reorder or omit anything that a message format requires, and MUST NOT accept weaker cryptography;
- timers stay within the bounds other documents set (for example keepalive below 60 s);
- memory caps never cause a different encoding, only refusals with the normal codes.

## 4. Testing

- **CI build.** Every change to the core is built for android/arm64 and android/amd64 with
  `CGO_ENABLED=0` (`go build` with that setting proves there is no cgo; `go vet` runs with the default
  toolchain settings, because on Go 1.27.3 `go vet` for android/amd64 fails with `CGO_ENABLED=0` while
  the build succeeds) and with
  `gomobile bind` for Android, and for ios/arm64 through `gomobile bind`, which links with the external
  linker the Go toolchain requires for iOS. A build failure fails CI. A check verifies that no EchoFog
  package imports `"C"`, lists every file with a build tag, and verifies that each tagged file has a
  fallback for android and ios (the builds themselves prove it compiles).
- **Conformance under the mobile profile.** The conformance test suite (the vectors in
  [vectors/](vectors/) and the interoperability tests between two nodes) runs twice: with the
  `desktop` profile and with `platform=android` and the mobile defaults of §2 (`low_power`, `metered`,
  no roles, `udp=restricted`, `mem_cap_mb=128`, best-effort memguard, no STUN or port mapping). Both
  runs must produce identical wire bytes for every deterministic vector and must interoperate with a
  desktop node.
- **Device runs.** Before a mobile release, an Android build runs on a real device and on the
  emulator (android/amd64) against desktop and server nodes: handshake, chat, mailbox fetch after
  suspension, hole punching, and the exit with a `VpnService` descriptor.
