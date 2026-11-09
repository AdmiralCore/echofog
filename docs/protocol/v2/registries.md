# Registries

Every number and string that appears on the wire or in a signed object is listed here. A value not
listed here MUST NOT be sent, except values from the experimental ranges between consenting
implementations. Adding an entry is a specification change; removing one is not allowed — a retired
value becomes **reserved** and is never reassigned.

Ranges marked *experimental* are for testing and MUST NOT appear in released builds' default
configuration.

## 1. Frame types (stream frames)

Frame layout: [wire.md](wire.md) §6. `I` = the frame has the IGNORABLE flag set when sent; `S` =
valid on the control stream (stream 0) only; `A` = valid on any stream; `H` = valid on stream 0
before the handshake completes; `B` = valid only on byte-stream carriers at connection level.

Session-level types are `0x00`–`0x3f`. Types `0x40`–`0x3fff` are **service-local**: their meaning
is given by the service of the stream they appear on (§4).

| Type | Name | Where | Payload | Max payload | Section |
|---|---|---|---|---|---|
| 0x00 | — | — | reserved, invalid | — | — |
| 0x01 | PADDING | A, B, I | any bytes (ignored) | 16384 | wire.md §6.4 |
| 0x02 | HS_HELLO | H | suite offer | 64 | handshake.md §3 |
| 0x03 | HS_MSG | H | one Noise message | 16384 | handshake.md §4 |
| 0x04 | HS_RETRY | H | puzzle challenge | 32 | handshake.md §6 |
| 0x05 | REJECT | H, S | `Reject` protobuf | 512 | handshake.md §8 |
| 0x06 | PING | S | `u64` opaque | 8 | wire.md §7 |
| 0x07 | PONG | S | `u64` echoed | 8 | wire.md §7 |
| 0x08 | GOAWAY | S | `GoAway` protobuf | 256 | wire.md §7 |
| 0x09 | CLOSE | A | `varint code ‖ reason (UTF-8, ≤ 200)` | 210 | wire.md §6.3 |
| 0x0a | OPEN | A (first frame of a non-control stream) | `varint service ‖ varint version ‖ u8 flags` | 24 | wire.md §6.3 |
| 0x0b | UNSUPPORTED | A | `varint service ‖ varint min ‖ varint max` | 24 | wire.md §6.3 |
| 0x0c | OBSERVED_ADDR | S | `u8 family ‖ addr (4/16) ‖ u16 port` | 19 | wire.md §7.2 |
| 0x0d | CARD_UPDATE | S | a signed node card | 4096 | objects.md §3.6 |
| 0x10 | PATH_BIND | H (instead of HS_HELLO) | `sid(16) ‖ varint path_id ‖ mac(32)` | 56 | wire.md §8.2 |
| 0x11 | PATH_BIND_OK | H | `varint path_id` | 8 | wire.md §8.2 |
| 0x12 | PATH_ABANDON | S | `varint path_id ‖ varint code` | 16 | wire.md §8.3 |
| 0x13 | PUNCH_OFFER | S | `PunchOffer { repeated bytes addrs = 1; uint64 nonce = 2; uint32 nat = 3; uint32 port_lo = 4; uint32 port_hi = 5; uint32 samples = 6; uint32 mode = 7; }`, each addr `family(1) ‖ ip ‖ port(2)`, ≤ 4 used; fields 3–7 are the `birthday-punch` additions | 256 | services.md §6.4 |
| 0x14 | PUNCH_SYNC | S | `u64 nonce` echoed; the sender starts RTT/2 later, the receiver at once | 8 | services.md §6.4 |
| 0x18 | MUX_STREAM | B | `varint stream_id ‖ u8 flags ‖ data` | 16400 | wire.md §5.2 |
| 0x19 | MUX_RESET | B | `varint stream_id ‖ varint code` | 16 | wire.md §5.2 |
| 0x1a | MUX_WINDOW | B | `varint stream_id ‖ varint increment` | 16 | wire.md §5.2 |
| 0x1b | MUX_MAX_DATA | B | `varint increment` | 8 | wire.md §5.2 |
| 0x1c | MUX_DATAGRAM | B | one datagram (§2 layout) | 1452 | wire.md §5.2 |
| 0x3e | GREASE | A, I | random bytes; never assigned, always skipped | 256 | registries.md §5 |
| 0x3f | — | — | reserved for a future extension-header frame | — | — |

Every other value in `0x00`–`0x3f` is unassigned. On receipt, `0x00`, `0x3f`, GREASE `0x3e` and every
unassigned value are unknown types: skipped with IGNORABLE; otherwise the receiver closes the
**stream** with `CLOSE{code = 2 unknown-frame}`, on stream 0 ends the session with `GOAWAY{code = 2}`,
and at mux connection level closes the carrier. Unknown types are never abuse. Frames that a feature
introduces count as unknown when the feature was not negotiated; known types outside their "Where"
column are protocol violations ([wire.md](wire.md) §6.1).

## 2. Datagram types

Datagram layout: `varint type ‖ payload` ([wire.md](wire.md) §6.5).

| Type | Name | Payload | Section |
|---|---|---|---|
| 0x00 | — | reserved | — |
| 0x01 | PADDING | ignored | wire.md §6.5 |
| 0x02 | SEALED | a sealed packet of a relayed session path | wire.md §8.4 |
| 0x10 | CALL_AUDIO | call media packet | services.md §9 |
| 0x11 | CALL_VIDEO | call media packet | services.md §9 |
| 0x12 | CALL_SIGNAL | lossy call signal (duplicated on the call stream) | services.md §9 |
| 0x13 | CALL_AUTH | batch authenticator of call media units | services.md §9.3 |
| 0x20 | EXIT_UDP | `varint session ‖ exit UDP fragment` | services.md §7 |
| 0x30 | RELAY_DGRAM | `varint circuit_id ‖ payload` | services.md §6 |

Unknown datagram types are dropped silently and counted. Datagrams are never answered with errors.

## 3. Services (L3)

| ID | Name | Versions in v2 | 0-RTT safe | Section |
|---|---|---|---|---|
| 0 | — | reserved | — | — |
| 1 | `dht` | 2 | FIND_NODE, FIND_VALUE only | services.md §2 |
| 2 | `pex` | 2 | GET only | services.md §3 |
| 3 | `chat` | 2 | no | services.md §4 |
| 4 | `mailbox` | 2 | no | services.md §5 |
| 5 | `relay` | 2 | no | services.md §6 |
| 6 | `exit` | 2 | no | services.md §7 |
| 7 | `files` | 2 | no | services.md §8 |
| 8 | `call` | 2 | no | services.md §9 |
| 9 | `names` | reserved | — | crypto usernames, later |
| 10 | `sync` | reserved | — | group log sync as its own service, later |
| 11 | `cluster` | 2 | no | cluster.md §4 |
| 0xf000–0xffff | — | experimental | — | — |

The text form of a service version is `name/version`, for example `chat/2`. On the wire a service is
always the numeric pair (`OPEN{service = 3, version = 2}`).

## 4. Service-local frame types

Service-local types start at `0x40`. Each service's table in [services.md](services.md) gives every
type's body and **maximum payload size** (the "Max" column, normative); they are collected here for
uniqueness. A type without a stated maximum does not exist.

| Service | Types |
|---|---|
| dht/2 | 0x40 FIND_NODE, 0x41 NODES, 0x42 FIND_VALUE, 0x43 VALUE, 0x44 STORE, 0x45 STORED, 0x46 ERROR |
| pex/2 | 0x40 GET, 0x41 CARDS |
| chat/2 | 0x40 DELIVER, 0x41 DELIVERED, 0x42 BUNDLE_GET, 0x43 BUNDLE |
| mailbox/2 | 0x40 QUEUE_CREATE, 0x41 QUEUE_OK, 0x42 DEPOSIT, 0x43 DEPOSITED, 0x44 FETCH, 0x45 ITEMS, 0x46 ACK, 0x47 FORWARD, 0x48 ERROR |
| relay/2 | 0x40 CONNECT, 0x41 INCOMING, 0x42 ACCEPT, 0x43 REFUSE, 0x44 CONNECTED, 0x45 REFUSED, 0x46 REGISTER, 0x47 REGISTERED, 0x48 PEER_ADDR |
| exit/2 | 0x40 TCP_CONNECT, 0x41 RESULT, 0x42 DATA, 0x43 UDP_OPEN, 0x44 UDP_OK, 0x45 UDP_PKT, 0x46 ACCOUNT_GET, 0x47 ACCOUNT, 0x48 VOUCHER, 0x49 VOUCHER_RESULT, 0x4a PASS, 0x4b PASS_RESULT |
| files/2 | 0x40 WANT, 0x41 CHUNK, 0x42 CANCEL, 0x43 DONE |
| call/2 | 0x40 SIGNAL |
| cluster/2 | 0x40 JOIN, 0x41 JOINED, 0x42 HEARTBEAT, 0x43 HEARTBEAT_ACK, 0x44 ASSIGN, 0x45 ASSIGNED, 0x46 REVOKE, 0x47 REPORT, 0x48 SYNC, 0x49 NOT_ACTIVE, 0x4a ERROR, 0x4b DESCRIPTOR_GET, 0x4c DESCRIPTOR |

## 5. Features (L2)

Feature IDs are `u16`. A feature changes session behaviour only when **both** sides list it; the
negotiated set is the intersection ([handshake.md](handshake.md) §7).

| ID | Name | Meaning |
|---|---|---|
| 0x0000 | — | reserved |
| 0x0001 | `path-migration` | PATH_BIND on further carrier connections and on relay circuits (wire.md §8) |
| 0x0002 | `multipath` | reserved: several active paths at once (later) |
| 0x0003 | `idle-padding` | the peer sends PADDING frames when idle, following its shape profile |
| 0x0004 | `observed-addr` | the responder reports the initiator's observed address (OBSERVED_ADDR, HsResp.observed) |
| 0x0005 | `early-data` | the responder processes 0-RTT data in HsInit.early |
| 0x0006 | `card-push` | either side may push a newer card with CARD_UPDATE |
| 0x0007 | `stream-resume` | reserved: streams that survive a path change by offset (successor of v1 TCP rotation) |
| 0x0008 | `birthday-punch` | hole punching through symmetric NATs with many ports; PUNCH_OFFER fields 3–7 (services.md §6.4.1) |
| 0x0009 | `wake-push` | reserved: a mailbox relay wakes a suspended mobile node through a push path (later, platforms.md §2) |
| 0x0010 | `spqr` | reserved: post-quantum ratchet in chat/2 (later) |
| 0x0011 | `sealed-header` | reserved: encrypted ratchet headers in chat/2 (later) |
| 0x0012 | `mls` | reserved: MLS groups (later) |
| 0x0013 | `cluster` | reserved: the peer is a member of an operator cluster and speaks `cluster/2` extensions (cluster.md) |
| 0x0a0a, 0x1a1a, 0x2a2a, …, 0xeaea | GREASE | never meaningful; see below |
| 0xf000–0xfeff | — | experimental (0xfafa is not GREASE) |

**Decision D18 (GREASE).** An initiator SHOULD include one randomly chosen GREASE value (`0xXAXA` for
X in 0..e) in its feature list, in `HS_HELLO.offered` (never as `chosen`) and as a service ID (with
version 1) in `HsInit.services` ([handshake.md](handshake.md) §3.4); responders MUST ignore GREASE
values and MUST NOT select them. Senders SHOULD also send, at random intervals, an IGNORABLE frame of a
GREASE frame type (`0x3e` on any stream) with random payload, which receivers skip. This keeps the
"unknown is ignored" rules exercised from day one, as TLS does. The same GREASE values are reserved in
the service registry (§3) and the L1 suite registry (§6.2).

## 6. Suites

### 6.1 L0 masking suites

L0 suite IDs appear only inside signed node cards (`Addr.suite`), never on the wire.

| ID | Name | Transport | Section |
|---|---|---|---|
| 0x0000 | — | reserved | — |
| 0x0001 | `noise3` | UDP | wire.md §2 |
| 0x0002 | `reality2-quic` | UDP | wire.md §3 |
| 0x0003 | `reality2-tcp` | TCP | wire.md §4 |
| 0x0004 | `ws2` | TCP (HTTPS WebSocket, CDN-friendly) | wire.md §4.4 |
| 0x0005 | `xhttp2` | reserved: HTTP/2 and HTTP/3 request streams instead of WebSocket | later |
| 0x0006 | `masque` | reserved: MASQUE CONNECT-UDP/IP | later |
| 0x0007 | `dns-tunnel` | reserved: last-resort DNS carrier | later |
| 0xff00–0xffff | — | experimental | — |

### 6.2 L1 handshake suites

| ID | Protocol name (exact, ASCII) | Section |
|---|---|---|
| 0x0000 | — reserved | — |
| 0x0001 | `EchoFog/2/Noise_XKhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s` | handshake.md §4.1 |
| 0x0002 | `EchoFog/2/Noise_XXhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s` | handshake.md §4.2 |
| 0xff00–0xffff | — experimental | — |

Responder preference order in v2: `0x0001` before `0x0002`. Values `0xXAXA` (X in 0..e) are GREASE.

### 6.3 Shape profiles

Built-in shape profile IDs (wire.md §9). A card may also carry a full profile as data.

| ID | Name |
|---|---|
| 0 | `none` (no padding beyond the suite minimum) |
| 1 | `default-udp` |
| 2 | `default-tls` |
| 3 | `constant-rate-call` (reserved for call media shaping) |
| 0x8000–0xffff | experimental |

## 7. Labels

Every label is unique. A label never changes meaning; a changed construction gets a new label.
Labels are ASCII, case-sensitive, without a terminating zero.

### 7.1 Signed-object labels (L5)

| Label | Object | Max body | Max lifetime | Section |
|---|---|---|---|---|
| `echofog/card/2` | node card | 3800 bytes (envelope ≤ 4096) | 7 days for scopes public and peer, 30 days for scope invite (by the `scope` field) | objects.md §3 |
| `echofog/prekeys/2` | chat prekey bundle (PQXDH) | 6144 | 30 days | objects.md §5 |
| `echofog/devices/2` | device list (multi-device, later use) | 4096 | 30 days | objects.md §6 |
| `echofog/mailbox-hint/2` | mailbox rendezvous hint | 6144 (envelope ≤ 6400) | 6 hours | objects.md §7 |
| `echofog/name/2` | crypto-username binding (format only; not stored in the v2 DHT) | 512 (envelope ≤ 768) | 30 days | objects.md §8 |
| `echofog/release/2` | release manifest with co-signatures | 20000 | 400 days | objects.md §9 |
| `echofog/cluster-master/2` | cluster master delegation (signed by the cluster root key) | 512 | 90 days | cluster.md §1.1 |
| `echofog/cluster/2` | cluster membership of one node | 512 | 3 days | cluster.md §1.2 |
| `echofog/cluster-pass/2` | cluster pass | 1024 | by term | cluster.md §7 |
| `echofog/cluster-desc/2` | cluster descriptor (member list; never public) | 16384 | 3 days | cluster.md §6.1 |

### 7.2 L0 labels

| Label | Use | Section |
|---|---|---|
| `echofog/noise3/keys` | HKDF info prefix of the noise3 key schedule (selector, Init, data and packet-number keys) | wire.md §2.3 |
| `echofog/reality2/token-key` | HKDF info prefix of the REALITY v2 token key | wire.md §3.2 |
| `echofog/reality2/hello` | hash prefix of the ClientHello binding | wire.md §3.2 |

### 7.3 L1 labels

| Label | Use | Section |
|---|---|---|
| `EchoFog/2/Noise_XKhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s` | Noise protocol name, suite 0x0001 | handshake.md §2 |
| `EchoFog/2/Noise_XXhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s` | Noise protocol name, suite 0x0002 | handshake.md §2 |
| `EXPORTER-echofog/hs2/channel` | TLS exporter label of the channel binding | handshake.md §3.2 |
| `echofog/hs2/circuit` | channel binding of a relayed session | handshake.md §3.2 |
| `echofog/hs2/prologue` | prologue prefix | handshake.md §3.3 |
| `echofog/hs2/pass-id` | invitation pass selector | handshake.md §5 |
| `echofog/hs2/access-proof` | invitation access proof | handshake.md §5 |
| `echofog/hs2/puzzle` | puzzle hash prefix | handshake.md §6 |
| `echofog/hs2/cookie` | puzzle challenge MAC | handshake.md §6 |
| `echofog/hs2/session-secret` | session secret from the final chaining key | handshake.md §9 |

### 7.4 L2 labels

| Label | Use | Section |
|---|---|---|
| `echofog/session2/id` | session ID derivation | handshake.md §9 |
| `echofog/session2/path-bind` | path-binding key derivation | wire.md §8.2 |
| `echofog/session2/path-mac` | PATH_BIND MAC input prefix | wire.md §8.2 |

### 7.5 DHT and service labels

| Label | Use | Section |
|---|---|---|
| `echofog/dht2/key` | DHT key of a signer-keyed record (with the day) | dht.md §2 |
| `echofog/dht2/release` | DHT key of the release manifest (with the day) | dht.md §2 |
| `echofog/release-cosign/2` | co-signature input prefix of a release manifest | objects.md §9 |
| `echofog/relay2/rendezvous` | rendezvous ID of an unlisted node at a relay | services.md §6.3 |
| `echofog/mailbox2/pow` | proof of work of a public-queue deposit | services.md §5.2 |
| `echofog/call2/unit` | hash prefix of a media unit in a batch authenticator | services.md §9.3 |
| `echofog/call2/batch` | signature input prefix of a batch authenticator | services.md §9.3 |
| `echofog/chat2/pqxdh` | HKDF info prefix of a PQXDH session key | services.md §4.0 |
| `echofog/chat2/ratchet-init` | HKDF info of the initial ratchet chains | services.md §4.0 |
| `echofog/chat2/ratchet-root` | HKDF info of the ratchet root KDF | services.md §4.0 |
| `echofog/storage2/key` | L6: local storage key from the seed | services.md §4.0 |
| `echofog/mailbox2/forward` | sealing of a forwarded deposit to the mailbox relay | services.md §5 |
| `echofog/mailbox2/fetch-auth` | signature input prefix of FETCH and ACK | services.md §5 |
| `echofog/chat2/seal` | sealed-sender envelope key | services.md §4 |
| `echofog/call2/sender-key` | per-sender media key | services.md §9 |
| `echofog/call2/signal` | signature input prefix of forwarded call signals | services.md §9 |
| `echofog/call2/epoch` | call epoch secret chain | services.md §9 |
| `echofog/cluster/id` | cluster ID from the root key | cluster.md §1 |

### 7.6 Retired v1 labels

**Decision D13 (chat cryptography labels, revised).** The end-to-end chat cryptography keeps its v1
constructions, but its labels move into the v2 scheme (§7.5): `FOG_PQXDH_X25519_SHA-256_ML-KEM-768_V1`
→ `echofog/chat2/pqxdh`, `FOG_RATCHET_INIT_V2` → `echofog/chat2/ratchet-init`, `FOG_RATCHET_ROOT_V2` →
`echofog/chat2/ratchet-root`, `FOG_STORAGE_KEY_V1` → `echofog/storage2/key`.

The v1 labels `ECHOFOG_PREKEY_BINDING_V1`, `ECHOFOG_PREKEY_BINDING_V2`, `echofog/handshake/v2/*`,
`echofog/access/v1/*`, `echofog/udp-obfs/v2`, `echofog/tcp-stealth/*`,
`echofog-reality-stealth-aead-v2`, `echofog-quic-mlkem-binding-v1`, `echofog-hello-binding-v1`,
`echofog/vpn-exit/v1/`, `echofog-mbox-v1`, `EXPORTER-echofog-peer-handshake`, the four `FOG_*` labels
above and `FOG_X3DH_V2`
(classic X3DH: v2 refuses classic inits) are **retired**: v2 MUST NOT use them. Labels that appeared in
drafts of this specification and were withdrawn — `echofog/noise3/selector` (the public-key selector,
K1), `echofog/dht2/name-key`, `echofog/exit2/pay-tag` — are reserved and never reused.

## 8. Algorithm tags (L7)

`akey = u8 alg ‖ key bytes`; the length is fixed by `alg`. Every key in a v2 message or object is an
akey (including masking keys), except fields whose format fixes the algorithm and size and says so:
`Prekeys.spk` (raw X25519, 32 bytes), `Prekeys.kem` (raw ML-KEM-768, 1184 bytes), and the raw keys inside
Noise handshake messages. Text form: `<name>:<base64url-nopad(key bytes)>`, for example
`ed25519:11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo` (the RFC 8032 TEST 1 public key).

| Tag | Name | Kind | Public key | Signature / ciphertext | Status |
|---|---|---|---|---|---|
| 0x00 | — | — | — | — | reserved |
| 0x01 | `ed25519` | signature (RFC 8032, pure) | 32 | 64 | v2 |
| 0x02 | `x25519` | key agreement (RFC 7748) | 32 | — | v2 |
| 0x03 | `mlkem768` | KEM (FIPS 203) | 1184 | ciphertext 1088 | v2 |
| 0x10 | `mldsa65` | signature (FIPS 204) | 1952 | 3309 | reserved |
| 0x11 | `ed25519+mldsa65` | hybrid signature: both MUST verify | 32 + 1952 | 64 + 3309 | reserved |
| 0xf0–0xff | — | experimental | — | — | — |

Only signature algorithms may sign objects. A receiver that meets an unknown tag treats the key as
unusable (the object is skipped, not an error for the whole message).

## 9. DHT record kinds

See [dht.md](dht.md). A record's kind is its object label. The key rule says how the DHT key is
derived; "signer" means `SHA-256("echofog/dht2/key" ‖ u32(day) ‖ u8 len(label) ‖ label ‖ signer_id)`, where `signer_id` is the
SHA-256 of the envelope signer's akey (the device ID for objects a device signs, the node ID for the account-signed
device list; [dht.md](dht.md) §2)
and "release" means `SHA-256("echofog/dht2/release" ‖ u32(day))`, with `day = floor(unix / 86400)`.

| Label | Key rule | Max record (envelope) | Max lifetime | Per signer | Replicas |
|---|---|---|---|---|---|
| `echofog/card/2` (public scope only) | signer | 4096 | 7 days | 1 | 8 |
| `echofog/prekeys/2` | signer | 8192 | 30 days | 1 | 8 |
| `echofog/devices/2` | signer | 6144 | 30 days | 1 | 8 |
| `echofog/mailbox-hint/2` | signer | 6400 | 6 hours | 1 | 5 |
| `echofog/name/2` | — not stored in v2 (squatting without an on-chain check) | — | — | — | — |
| `echofog/release/2` | release (threshold-signed) | 20480 | 400 days | 1 in total (highest `released_at`) | 20 |
| any other label starting `echofog/` | signer | 2048 | clipped to 24 hours on storage | 4 per signer | 5 |
| any label not starting `echofog/` | — not stored (the 2048-byte generic limit still applies wherever such an object is carried) | 2048 | — | — | — |

Peer- and invite-scope cards are never stored (objects.md §3.2).

## 10. Error and reason codes

### 10.1 REJECT reasons (handshake.md §8)

| Code | Name | `min`/`max` meaning |
|---|---|---|
| 0 | — | reserved |
| 1 | `version` | supported protocol versions |
| 2 | `suite` | — (the REJECT carries the supported suite IDs in `detail`) |
| 3 | `feature-required` | the required feature ID in `min` |
| 4 | `overloaded` | `retry_after` seconds |
| 5 | `policy` | — |
| 6 | `banned` | `retry_after` seconds, 0 = indefinite |
| 7 | `card-invalid` | — |
| 8 | `clock-skew` | the responder's time in `min` (unix seconds, low 32 bits) |
| 9 | `rate-limited` | `retry_after` seconds |
| 10 | `internal` | — |

A hidden node never sends REJECT before a valid access proof ([handshake.md](handshake.md) §5).

### 10.2 CLOSE, GOAWAY and PATH_ABANDON codes

| Code | Name |
|---|---|
| 0 | `no-error` |
| 1 | `protocol-violation` |
| 2 | `unknown-frame` |
| 3 | `frame-too-large` |
| 4 | `service-unsupported` |
| 5 | `refused` |
| 6 | `timeout` |
| 7 | `overloaded` |
| 8 | `going-away` (shutdown or restart) |
| 9 | `path-failed` |
| 10 | `internal` |
| 0x100–0xffff | service-defined (each service section lists its codes) |

### 10.3 relay/2 REFUSED reasons

| Code | Name |
|---|---|
| 1 | `unavailable` — unknown, unreachable, refused by the target or by policy: one answer for all, after a random delay (services.md §6.3) |
| 2 | `limit` — the requester exceeded its own circuit limits |

### 10.4 exit/2 RESULT codes

| Code | Name |
|---|---|
| 0 | `ok` |
| 1 | `denied` (policy or access) |
| 2 | `unreachable` (refused, timed out or reset — not distinguished, Decision D19) |
| 3 | `payment-required` |
| 4 | `busy` |
| 5 | `dns-failed` |
| 6 | `blocked-port` |
| 7 | `rate-limited` |

## 11. Node roles and card flags

| `NodeCard.role` | Name |
|---|---|
| 0 | `user` |
| 1 | `server` (always dialed through a camouflaged suite across borders) |
| other | treated as `user` (unknown values are ignored, not errors) |

| `NodeCard.flags` bit | Name | Meaning |
|---|---|---|
| 0 | `unlisted` | never pass this card to third parties (PEX, DHT answers); hidden and anonymous nodes |
| 1 | `relay` | offers relay/2 |
| 2 | `dht-server` | answers dht/2 for others |
| 3 | `mailbox` | holds mailbox/2 queues for others |
| 4 | `invite-scoped` | an invitation card: never published or forwarded |
| 5 | `exit` | the card's `exit` offer is live |
| 6 | `domestic-only` | a server that is never dialed across a border (no camouflaged address needed) |
| 7 | `cluster` | the card carries a `cluster` reference (cluster.md §2) |
| 8–31 | — | reserved: sent as zero, **ignored** on receipt (a later version may assign them) |

## 12. Clock endpoints

Default HTTPS endpoints for the clock check of [handshake.md](handshake.md) §7 (Decision D36). Each is
a very large CDN or platform front page, visited with `HEAD /`; the node uses the `Date` header only.
The list is configuration, not protocol: users and distributions may replace it, and a node needs at
least 3 answers and a strict majority agreeing within 2 s.

| Endpoint |
|---|
| `https://www.cloudflare.com/` |
| `https://www.akamai.com/` |
| `https://www.fastly.com/` |
| `https://aws.amazon.com/` |
| `https://www.microsoft.com/` |
| `https://www.apple.com/` |
| `https://www.google.com/` |
