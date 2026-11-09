# Wire format: L0 masking suites, frame v2, paths, shape profiles

This document specifies everything below the handshake and the frames that carry the session:

- §1 carrier selection;
- §2 `noise3`, the masked UDP suite for links inside one country;
- §3 `reality2-quic` and §4 `reality2-tcp` / `ws2`, the camouflaged suites for links across a border
  and for every link to a server;
- §5 the inner TLS and the byte-stream mux;
- §6 frame v2; §7 session control; §8 paths; §9 padding and shape profiles; §10 limits and timeouts.

Byte layouts use the conventions of [README.md](README.md) §6. Test vectors:
[vectors/noise3.json](vectors/noise3.json), [vectors/elligator2.json](vectors/elligator2.json),
[vectors/reality2.json](vectors/reality2.json), [vectors/frames.json](vectors/frames.json),
[vectors/varint.json](vectors/varint.json), [vectors/kdf.json](vectors/kdf.json).

## 1. Carrier selection

An L0 carrier is chosen from the responder's **signed card** only. Addresses, suites, masking keys and
suite parameters (SNI, WebSocket URL, shape profile) learned from anywhere else — unsigned address
annotations, third-party PEX entries, DHT routing replies — MUST NOT be used to choose a suite or its
parameters (K4). Bootstrap entries MUST carry at least the node ID (better the whole card); a bare
`host:port` without either can be dialed only with the XX bootstrap handshake under a throwaway
identity ([handshake.md](handshake.md) §4.2), and nothing learned through it is trusted.

**Decision D21 (mesh carrier).** The initiator selects the suite as follows.

1. If the responder's card has `role = server`, the initiator MUST use a camouflaged suite
   (`reality2-quic`, `reality2-tcp` or `ws2`), except when both nodes are positively known to be in the
   same country (see 3). A card with flag `domestic-only` is never dialed across a border. A server card without a camouflaged address is not dialed directly across a
   border; it is reached through a relay.
2. If the initiator is in the same country as the responder and holds a peer- or invite-scope card
   with a noise3 address (§2.1), it MAY use `noise3` on a path that meets the path requirement of §2.4.
3. "Same country" requires positive knowledge: both public addresses map to the same country in the
   node's built-in offline GeoIP database. An unknown country counts as a border. A node MUST NOT query
   an online GeoIP service with peer addresses (K7). The initiator's **own** country may come from the
   user's explicit setting (desktop) or the platform hint (Android); when neither is set, GeoIP of the
   client's first public address is the default. The **responder's** country always comes from the
   built-in offline GeoIP database; a peer's own claim never overrides it.
4. Across a border, or when unknown, the initiator MUST use a camouflaged suite; if the responder has
   none (an ordinary user), the link goes through a relay that has one (guard mode, as in v1).
5. Happy eyeballs: the preferred suite is tried first; the second starts after
   `300 ms + uniform(0, 200) ms` or immediately when the first fails. Per-address and per-network
   memory of the working suite is kept as in v1.
6. A TCP suite is dialed only on the port its card lists. No suite is dialed with a guessed port, and
   no REALITY suite is dialed with an SNI that is not in the card (no built-in default SNI; V1).

## 2. noise3

noise3 makes every datagram of a flow indistinguishable from uniformly random bytes to every observer
who does not know the initiator's ephemeral secret or the responder's masking **private** key. Knowing
the responder's card, including its masking public key, does not let a passive observer recognise a
flow (K1). It wraps the datagrams of an inner QUIC v1 connection (§5).

**Byte order (applies to all of §2).** README §6 makes integers big-endian unless a field says `le`. In
this section the names below are exact:

| Notation | Size | Byte order |
|---|---|---|
| `u8` | 1 | — |
| `u16`, `u32` | 2, 4 | big-endian (network order): `inner_len`, `ts`, the AEAD AAD `u32(n mod 2^32)` |
| `u32le`, `u64le` | 4, 8 | little-endian: the ChaCha20 counter `u32le(sample[0:4])`, the nonce `u64le(n)` |
| `LE(x)`, `LE32(x)` | 32 | little-endian field elements (§2.2) |

### 2.1 Keys and where they are published

- The responder has one or more **masking keys**: X25519 key pairs `(mask_sk, mask_pub)`, generated
  at random, never derived from the identity. Each appears in a card as a `MaskKey`
  ([objects.md](objects.md) §3) with `not_after`.
- **noise3 keys are never published.** noise3 addresses and their masking keys appear only in cards of
  scope `peer` or `invite` ([objects.md](objects.md) §3.2), which are delivered inside sessions and
  invitations and never stored in the DHT or passed in PEX. Public cards list only camouflaged
  addresses (§3, §4). So a censor who crawls the DHT learns no noise3 address or key at all.
- **Decision D37 (stranger-reachable servers need a cover).** A consequence: strangers can reach a node
  only through a camouflaged suite. Every server that accepts strangers (any node with a public card)
  MUST run `reality2-quic` or `reality2-tcp` with a cover site, or `ws2` behind a CDN. The installer
  finds a cover automatically (§3.5); a node without one publishes no public card and serves only peers
  and invitees.
- A hidden node uses a **separate masking key per unredeemed invitation**, and one shared
  **contacts key** (rotated at least every 30 days) for contacts that have completed a session
  ([objects.md](objects.md) §10). Because the selector is keyed on the ECDH secret (§2.3), sharing a key
  among contacts does not let one contact recognise another's flows passively.
- **Decision D10 (masking-key limits, revised).** A noise3 listener (one UDP port) MUST NOT hold more
  than 8 live masking keys, and a REALITY v2 listener not more than 8. Both suites need one X25519 per
  live key to test a stranger's first packet. A node that needs more keys (many pending invitations)
  opens further listeners. Stranger first packets are processed within a budget of 50 per second per
  source IP and 2000 per second per listener; beyond it they are dropped unprocessed, which is also
  what a random UDP service would appear to do.
- A key's `not_after` is an upper bound only: the node stops accepting a key as soon as it revokes it
  (for example when the last invitation using it is revoked). `not_after` MUST NOT exceed the
  `not_after` of the card that lists the key by more than 7 days.

### 2.2 Ephemeral key and Elligator2 representative

The initiator encodes its ephemeral public key with Elligator2 so that it looks uniformly random.

Constants: `p = 2^255 − 19`, `A = 486662`, `Z = 2` (a non-square mod p). Field elements are encoded as
32-byte little-endian integers.

**Dirty ephemeral key.** A plain X25519 public key is always in the prime-order subgroup, and that is
detectable after Elligator2 decoding. The initiator therefore makes the public point uniform on the
whole curve:

```
sk  = 32 random bytes (CSPRNG)
s   = clamp(sk)                              RFC 7748: s[0] &= 248; s[31] &= 127; s[31] |= 64
t   = sk[0] & 7                              (the three bits clamping discards)
P   = [s]B + [t]T                            on edwards25519; B = base point, T = a point of order 8
u_e = Montgomery u-coordinate of P           u = (1 + y) / (1 − y)
```

`T` is the order-8 point with encoding
`c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a` (any order-8 point gives the same
security; this one is fixed for the vectors). Because every clamped X25519 scalar is a multiple of 8,
`X25519(x, u_e) = X25519(x, u_clean)` for every peer scalar `x`: the torsion component changes the
encoding, not the shared secret.

**Inverse map (encoding).** Given `u = u_e` and a byte `tweak` drawn from a CSPRNG independently of
`sk`:

1. If `u = 0`, or `u = −A`, or `−2·u·(u + A)` is not a square: no representative exists. Pick a new
   `sk` and start again (about half of all keys fail).
2. If `tweak & 1 = 0`: `r = sqrt(−u / (2·(u + A)))`, else `r = sqrt(−(u + A) / (2·u))`.
3. Of the two roots `r` and `p − r`, take the one that is `≤ (p − 1)/2`.
4. `repr = LE32(r)`; then `repr[31] |= tweak & 0xc0` (the two top bits are random).

**Forward map (decoding)**, as in RFC 9380 §6.7.1 for curve25519:

1. `r = LE(repr)` with bits 254 and 255 cleared.
2. `x1 = −A / (1 + 2·r²)`.
3. `g(x) = x³ + A·x² + x`. If `g(x1)` is a square, `u = x1`; otherwise `u = −x1 − A`.

The case `1 + 2r² = 0` cannot occur. For `p = 2^255 − 19` the value −2 is not a square modulo p
(p ≡ 5 mod 8), so the denominator in step 2 is never zero and no separate branch is needed.

Two maps are meant and must not be confused. The **element map** `E(r)` is the RFC 9380 §6.7.1 map of a
full field element `r ∈ [0, p)`: steps 2 and 3 above with `r` as given. The **wire map** `W(repr)` is
`E(LE(repr))` with bits 254 and 255 cleared, which is steps 1–3 above. The RFC cross-check applies `E` to
full field elements; the check on generated keys applies `W` to the wire bytes.

Implementations MUST check, for every generated key, that the wire map `W(repr)` returns `u_e`.

### 2.3 Key schedule

```
z      = X25519(sk, mask_pub)               initiator            (all-zero z: abort)
       = X25519(mask_sk, u)                 responder, u = forward_map(repr)
okm    = HKDF-SHA256(ikm = z, salt = empty,
                     info = "echofog/noise3/keys" ‖ mask_pub(32) ‖ u_e(32), L = 192)
k_sel  = okm[0:32]      selector key
k_init = okm[32:64]     key of the Init datagram
k_i2r  = okm[64:96]     data packets initiator → responder
k_r2i  = okm[96:128]    data packets responder → initiator
hp_i2r = okm[128:160]   packet-number protection key initiator → responder
hp_r2i = okm[160:192]   packet-number protection key responder → initiator
sel    = HMAC-SHA256(k_sel, repr)[0:16]
```

The selector depends on `z`. Computing it requires `sk` (known only to the initiator) or `mask_sk`
(known only to the responder); `mask_pub` alone is not enough. The selector is a cheap pre-check before
the AEAD over the whole datagram; it gives a passive observer nothing.

**Decision D2 (hash families).** L0, L2, signed objects, links and services use SHA-256, HMAC-SHA256
and HKDF-SHA256 (hardware support, standard libraries everywhere). BLAKE2s is used only inside the
Noise handshake, because the agreed suite name fixes it there.

**Decision D11 (no forward secrecy at L0).** The noise3 keys depend on the responder's static masking
key. Whoever later steals `mask_sk` can remove the L0 layer of recorded flows and see the inner QUIC
packets, which remain protected by TLS 1.3 and the Noise session, and can recognise those recorded
flows after the fact. A node whose `mask_sk` may have been stolen (for example a seized VPS) MUST
rotate both its masking keys and its handshake key `hs`. L0 is for unobservability, not
confidentiality; an extra round trip for an L0 ephemeral exchange is not worth it.

### 2.4 The Init datagram

The first datagram of a flow, initiator → responder:

| Offset | Size | Field | Content |
|---|---|---|---|
| 0 | 32 | `repr` | Elligator2 representative of the ephemeral key (§2.2) |
| 32 | 16 | `sel` | selector (§2.3) |
| 48 | n + 16 | `box` | `ChaCha20-Poly1305(k_init, nonce = 0^12, aad = repr ‖ sel, InitPlain)` |

```
InitPlain = ts(u32, unix seconds) ‖ flags(u8 = 0) ‖ inner_len(u16) ‖ inner ‖ zero padding
```

- `inner` is one inner QUIC datagram (a QUIC Initial). The inner QUIC stack MUST be configured to send
  Initial datagrams of exactly 1200 bytes.
- The total length is `max(drawn, 71 + inner_len)`, where `drawn` comes from the shape profile (§9;
  default uniform in [1280, 1420]), and is never more than the path MTU minus IP and UDP headers
  (1452 for IPv4 on a 1500-byte path; the path requirement of noise3 is stated below). If the
  inner datagram does not fit, the sender takes the largest allowed length; it never splits an inner
  datagram.
- `flags` bits are reserved; senders MUST send 0; receivers MUST ignore them (they are inside the
  AEAD, so they cannot be used to fingerprint).
- Overhead: 71 bytes plus padding.

**Path requirement (`ErrPathTooSmall`).** An Init carries 71 bytes of header and a 1200-byte inner QUIC
Initial, so noise3 needs a path that carries at least **1271 bytes** of UDP payload: an IPv4 path MTU of
at least 1299, or an IPv6 path MTU of at least 1319. The universal floor of **1232 bytes** of UDP
payload applies to the other carriers only. It is not a reason to shrink the inner Initial or the path
requirement: 1271 stands. An Init is never split across datagrams. If the initiator learns that the path
cannot carry 1271 bytes (an ICMP Packet Too Big message, a known path MTU below that, or no answer to an
Init after its retries), it marks noise3 unusable on that path and **falls back to the TCP carriers**
(`reality2-tcp` or `ws2`, §1) without waiting for a full handshake timeout. A sender whose datagram
ceiling is below 1271 bytes fails with `ErrPathTooSmall` for noise3.

Until it has received an authenticated responder packet, the initiator sends every outgoing inner
datagram as a **fresh** Init (new ephemeral key, new `repr`), so retransmissions never repeat bytes
on the wire. It keeps the keys of up to 4 outstanding Inits.

The Init box uses the nonce `0^12` under `k_init`. This is safe only because `k_init` is derived from a
fresh ephemeral key for every Init, so no (key, nonce) pair repeats. Reusing an ephemeral key for a second
Init is **MUST NOT**.

### 2.5 Responder processing of an Init

A datagram that does not belong to an established flow (§2.6) is treated as a candidate Init:

1. If its length is below 71 + 1200, drop.
2. Within the stranger budget (§2.1): decode `u`; for each live masking key compute `z` and `okm`,
   and compare `sel` in constant time. No match: drop. All-zero `z`: drop.
3. Open `box` with `k_init`; failure: drop.
4. `|ts − now| > 300 s`: drop.
5. Replay cache keyed by `sel`, kept for 900 s: a hit is dropped. The cache SHOULD be persisted across
   restarts (a Bloom filter flushed at least every `uniform(8, 12)` s), so a restart does not reopen a
   replay window.
6. **Flow choice.** The flow key is the source IP:port. If no flow exists for it, create one with this
   Init's keys (a *pending* flow). If a pending flow exists for it (an earlier Init within the last
   10 s, no data packet received yet), this Init is a *sibling*: its `inner` is delivered to the same
   flow and its keys are kept as alternates. The responder sends all its packets with the keys of the
   **first** Init of the flow; the initiator, which holds all its outstanding key sets, adopts whichever
   set the first authenticated responder packet uses and discards the others.
7. Limits: 4096 pending flows in total, 64 per source IP; when full, a **random** pending flow is
   evicted (an attacker cannot predict which honest flow to push out).
8. Pass `inner` to the inner QUIC stack. The inner stack sees one stable virtual address per flow, so
   NAT rebinding does not disturb it.
9. Until the inner QUIC handshake of the flow is confirmed, the responder's L0 MUST NOT send more than
   3 times the bytes it received on the flow (anti-amplification), and under load the inner QUIC SHOULD
   require address validation with a QUIC Retry, which travels inside noise3 and is invisible on the
   wire.

Every failure is a silent drop. A noise3 port never sends anything in reply to an unauthenticated
datagram (V1).

### 2.6 Data packets

All later datagrams of the flow, in both directions:

| Offset | Size | Field | Content |
|---|---|---|---|
| 0 | 4 | `pn` | the low 32 bits of the packet number `n`, masked |
| 4 | m + 16 | `box` | `ChaCha20-Poly1305(k_dir, nonce = 0^4 ‖ u64le(n), aad = u32(n mod 2^32), DataPlain)` |

```
DataPlain = inner_len(u16) ‖ inner ‖ zero padding          (inner_len = 0: a padding-only packet)
sample    = box[0:16]
mask      = ChaCha20(hp_dir, counter = u32le(sample[0:4]), nonce = sample[4:16]) applied to 4 zero bytes
pn        = u32(n mod 2^32) XOR mask
```

This is QUIC's header protection (RFC 9001 §5.4.4) applied to a 4-byte packet number.

- `dir` is `i2r` or `r2i`; `n` is the packet number of that direction, starting at 0 and increasing by
  one per packet (the Init does not count). A sender MUST NOT send with `n ≥ 2^62`.
- The receiver unmasks `pn` and reconstructs the full `n` as in RFC 9000 Appendix A.3 with 32-bit
  encoding: `expected_pn = largest_pn + 1`, where `largest_pn` is the highest packet number authenticated
  so far in this direction, and `n` is the value congruent to `pn` modulo 2^32 in the window
  `[expected_pn − 2^31, expected_pn + 2^31)`. Before the first authenticated packet of a direction,
  `largest_pn` is undefined and `expected_pn = 0`. Then it opens `box`. Any number of lost packets in a row
  can be recovered from, since the reconstruction window is ±2^31 packets. A replay bitmap covers the last
  1024 numbers below the highest; older or duplicate numbers are dropped.
- **Finding the flow.** By source IP:port. A packet from an unknown IP:port is tried, within the
  stranger budget, against the established flows of the same source IP and then against the 8 most
  recently active flows that have been silent for more than 1 s (NAT rebinding); otherwise it is a
  candidate Init (§2.5).
- **Reply address.** The responder replies to the flow's current address. It moves to a new address
  only after 2 authenticated packets from that address with `n` above every number received from the
  old one, and then sends at most 3 times the bytes received from the new address until the next
  authenticated packet arrives from it (an on-path attacker who races one copied packet cannot
  redirect the flow).
- **Dead flows.** If the initiator receives no authenticated packet for `uniform(12, 18)` s while it has
  been sending, it starts a new flow with a new Init (new ephemeral). The inner QUIC connection
  continues on it: to the responder's inner stack the new flow is a new path of the same QUIC
  connection, and QUIC's own path validation applies.
- Overhead: 22 bytes plus padding. Flows idle for 120 s are deleted.

### 2.7 Junk and decoys

Before the first Init, the initiator MAY send junk datagrams and decoys as its shape profile says
(§9): random-byte datagrams, or datagrams shaped like a DNS query or a QUIC Initial of another
connection. The responder drops them at step 2 of §2.5. They exist only to change the first-packet
shape seen by classifiers; they carry nothing. The first Init itself remains a fully random datagram
of at least 1271 bytes; a profile that makes the Init itself look like a QUIC long-header packet is
future work ([security.md](security.md) §4).

## 3. reality2-quic

The responder runs on UDP (port 443 RECOMMENDED) next to a real HTTP/3 website (the **cover**). A
client that knows the responder's masking key produces a QUIC Initial whose ClientHello carries an
authentication token; every other packet is forwarded to the cover, which answers it. Unlike noise3,
REALITY addresses and keys may be published: they protect against recognition by looking like the
cover, not by secrecy of the key.

### 3.1 Client hello

- QUIC v1, ClientHello shaped like the current stable Chrome: GREASE, `X25519MLKEM768` and `X25519`
  key shares, the Chrome extension set and order rules, ALPN `h3`, Chrome transport parameters. The
  SNI is `Addr.params.sni` from the card. There is no fallback SNI.
- The client's X25519 key share (the `X25519` entry; if absent, the last 32 bytes of the
  `X25519MLKEM768` entry) has private key `c` and public key `C`. `c` MUST be fresh for every
  ClientHello.
- The token is placed in `ClientHello.random` (32 bytes). QUIC forbids a non-empty
  `legacy_session_id`.

### 3.2 Token

```
z     = X25519(c, mask_pub)                                          all-zero: abort
k_tok = HKDF-SHA256(ikm = z, salt = empty,
                    info = "echofog/reality2/token-key" ‖ mask_pub(32) ‖ C(32) ‖ u8(len(a)) ‖ a, L = 32)
aad   = SHA-256("echofog/reality2/hello" ‖ CH0)
pt    = 0x01 ‖ ts(u32, unix seconds) ‖ 0x00 × 11                      16 bytes
token = AES-256-GCM(k_tok, nonce = 0^12, pt, aad)                     16 + 16 = 32 bytes
```

- `a` is the address the client dialed, exactly as written in the card: `host:port` for
  reality2-quic, reality2-tcp and ws2 (`Addr.host`, `":"`, decimal `Addr.port`). For ws2 this is the
  `host:port` component of `Addr.url`; the scheme, path and query are not part of `a`. The token is
  therefore valid only for that address: a captured hello cannot be replayed at another address of the
  same node to link the two. The server tries each address of its live cards that uses the masking key
  (z is computed once; each try is one HKDF).
- `CH0` is the ClientHello handshake message (handshake type, 3-byte length, body: the bytes that
  enter the TLS transcript) with the 32-byte token field set to zero. The fixed nonce is safe because
  `k_tok` is used once: `c` is fresh per ClientHello. Binding `aad` to the whole ClientHello stops an
  attacker from moving a captured token into a ClientHello with other key shares.

### 3.3 Server gate

For every UDP packet:

1. A packet from an **admitted 4-tuple** (source address and port) goes to the node's QUIC stack.
   Admission is by the 4-tuple only; the Destination Connection ID does not admit a packet from
   another source. A live connection whose source port changes (NAT rebinding) is therefore not
   admitted by its DCID: its packets go to the cover, and the flow does not survive the change. Binding
   the gate to the DCID of a live admitted connection would keep such a flow alive; that is a known
   limitation for the audit, not a rule of v2.0.
2. A QUIC v1 Initial from an unadmitted source: buffer the flight (at most 256 pending hellos, 8 KiB per
   source, 500 ms) until the ClientHello is complete. For each live masking key (≤ 8): compute `z`,
   then `k_tok` for each candidate address, and open the token. Valid, `pt[0] = 0x01`, zero padding,
   `|ts − now| ≤ 300 s`, and not in the replay cache → admit the source and pass the flight to the QUIC
   stack. Otherwise → forward the whole flight to the cover.
3. **Everything else** — other versions, long headers that are not v1 Initials, garbage, and hellos
   beyond any limit of step 2 — goes to the cover. Nothing is ever dropped because of a node-side
   limit. The node MUST NOT send Version Negotiation or a stateless reset to an unadmitted source, and
   MUST NOT send Retry before step 2 succeeds (V1).
4. Replay cache: the 32-byte tokens, kept 900 s, shared by every listener that uses the same masking
   key, persisted across restarts (Bloom filter).

Cover forwarding uses one outbound UDP socket per stranger source, at most 1024; when full, only
forwarding sessions idle for more than 30 s are evicted, and a new stranger beyond that is forwarded
through a shared socket. A live forwarded connection is never cut by the node (a real site would not
cut it either).

### 3.4 After the gate: mirroring the cover

The node's own TLS server is visible to an observer up to the encrypted part, and its encrypted
records are visible by size. It MUST look like the cover's:

- **Cover profile.** At start-up and then every `uniform(5, 7)` hours, the node connects to the cover
  with the same Chrome-shaped ClientHello its clients use and records the **cover profile**: selected
  TLS group, cipher suite, ServerHello extension order, whether HelloRetryRequest is used, ALPN chosen,
  QUIC server SCID length and the observable shape of the server's first flight (coalescing, packet
  sizes), the exact length of the Certificate message and of the other encrypted handshake messages,
  and the number and sizes of NewSessionTicket messages.
- The node's server MUST reproduce every observable item of the profile: the same group and cipher
  suite, the same SCID length, the same extension order, a certificate message of **exactly** the same
  length (padded with a private extension), the same ticket behaviour. If it cannot reproduce an item,
  it MUST NOT enable the suite with that cover.
- The node SHOULD, for each admitted connection, also open a connection to the cover with the client's
  ClientHello and copy the sizes of the cover's post-handshake records into its own first records
  (as Xray REALITY does), so that an intercept-and-replay prober sees the same record pattern.
- The certificate is self-signed for the SNI, with no organisation or other identifying field
  (no `O=EchoFog`).
- **Post-quantum confidentiality (Decision D5, revised).** If the cover selects `X25519MLKEM768`, the
  node selects it too, and the session's frames on this path travel in TLS as usual. If the cover
  selects a classical group, the node selects the same classical group, and the path runs in **sealed
  mode** (§8.5): every frame is additionally sealed with the hybrid Noise transport keys. Either way
  bulk data keeps post-quantum confidentiality, and the ServerHello matches the cover.
- ALPN is `h3` on the wire; the content of the connection is frame v2, not HTTP/3.
- The cover MUST be a site that really serves HTTP/3 on the same port: the start-up profile check
  includes a completed HTTP/3 handshake.
- **Residual:** an on-path attacker who intercepts a client's real ClientHello and sends it himself
  within the timestamp window is admitted (the node sees the hello for the first time) and can
  compare the node's answer with the cover's. Mirroring makes the two answers alike up to the end of
  the handshake; it does not make them identical afterwards ([security.md](security.md) §4).

### 3.5 Finding a cover automatically

The installer and every node at start-up select the cover without operator work, as v1 nodes already
search their neighbourhood:

1. Candidates: the operator's configured site, if any; otherwise hosts in the node's own /24 (IPv6 /48)
   and nearby /24s found by probing port 443, as in v1's neighbour search, never more than 1 probe per
   second.
2. A candidate qualifies if it serves HTTPS on TCP 443 and HTTP/3 on UDP 443 with a certificate valid
   for an SNI that resolves to it, and its cover profile (§3.4) can be reproduced. For `reality2-tcp`
   alone, HTTPS on TCP is enough.
3. Prefer covers that select `X25519MLKEM768` (no sealed mode needed) and that are popular in the
   node's country.
4. The chosen cover is re-checked with the profile measurement every `uniform(5, 7)` hours; if it stops
   qualifying, the node picks another and publishes a new card. If no cover qualifies, the node keeps
   serving peers and invitees and tells the operator that it is not reachable by strangers.

## 4. reality2-tcp and ws2

### 4.1 reality2-tcp client

TLS 1.3 over TCP with a ClientHello shaped like the current stable Chrome (uTLS), SNI from the card,
ALPN `h2, http/1.1`. The token (§3.2, same formula) goes into the 32-byte `legacy_session_id`;
`ClientHello.random` stays random. `C` is the client's X25519 share as in §3.1.

### 4.2 reality2-tcp server

- Reads the first TLS record (≤ 16 KiB, within 10 s). Valid token (§3.2 and the checks of §3.3 step 2)
  → completes TLS 1.3 itself, mirroring the cover profile of §3.4 (measured over TCP, with ALPN as the
  cover selects it). Anything else → transparent TCP proxy to the cover, including the bytes already
  read.
- At most 256 pre-token handshakes at once; the slot is released as soon as the token is checked, so
  slow strangers cannot exhaust the node (V4). Excess connections are proxied to the cover without
  being read, never reset.
- Per-address limits apply only after a valid token.

### 4.3 TLS records and padding

reality2-tcp carries the byte-stream mux (§5.2) inside TLS application data. Record sizes follow the
shape profile: PADDING frames (§6.4) fill records during the padding budget. This is the only padding
mechanism on TCP carriers; there is no separate vision layer (V6).

### 4.4 ws2

For networks that only allow HTTPS through a CDN or a reverse proxy.

- `Addr.url` is a `wss://` URL. The outer TLS (to the CDN) MUST use a browser ClientHello (uTLS); the
  Go standard TLS stack MUST NOT be used for it.
- The path SHOULD contain an unguessable component taken from the card; other paths of the host serve a
  normal site. ws2 addresses SHOULD appear only in peer- and invite-scope cards: a published path lets
  a scanner see that a WebSocket upgrade succeeds there.
- Inside the WebSocket (binary messages; one message per write, ≤ 16 KiB) runs the inner TLS of
  reality2-tcp, including the token, with SNI `Addr.params.sni`.
- An inner ClientHello without a valid token: the server answers as a generic WebSocket back end that
  does not understand the message: it closes the WebSocket with close code 1008 (policy violation)
  after `uniform(0.05, 0.5)` s.

## 5. Inner TLS and the byte-stream mux

### 5.1 Inner TLS

Every carrier has exactly one TLS 1.3 session between the two nodes:

| Suite | TLS session |
|---|---|
| `noise3` | QUIC v1 TLS inside the noise3 flow |
| `reality2-quic` | the QUIC v1 TLS admitted by the token |
| `reality2-tcp` | the TLS admitted by the token |
| `ws2` | the inner TLS inside the WebSocket |

Rules for every inner TLS:

- certificates are not verified (authentication is the Noise handshake, bound to this TLS session by
  the exporter, [handshake.md](handshake.md) §3.2);
- the noise3 inner certificate is self-signed with an empty subject and a random serial; ALPN `h3`. Its
  key is **ECDSA P-256** by default. An Ed25519 key is used only when the cover profile offers `ed25519`
  in its signature algorithms; otherwise ECDSA P-256 is kept. The server serves only a key type that the
  ClientHello actually offered;
- noise3: the key exchange group MUST be `X25519MLKEM768`. REALITY suites: the group mirrors the
  cover (§3.4); when it is classical the path runs in sealed mode (§8.5). Each side checks the
  negotiated group and refuses a classical group on a path that is not in sealed mode;
- TLS and QUIC early data (0-RTT) MUST NOT be used. 0-RTT exists at L1 instead
  ([handshake.md](handshake.md) §10);
- QUIC transport parameters (noise3): idle timeout 60 s, `max_udp_payload_size` reduced by the L0
  overhead plus the profile's maximum padding, datagrams enabled, at most 256 bidirectional and
  **64 unidirectional** streams. The 64 is the Chrome value: a limit of 0 would be a visible
  difference from the browser's transport parameters. QUIC's own keepalive is off; L2 PING is used
  instead (§7.1).

On QUIC carriers, v2 streams are QUIC streams (stream 0 is the first client bidirectional stream) and
v2 datagrams are QUIC DATAGRAM frames.

### 5.2 Byte-stream mux (TCP carriers and relay stream circuits)

**Decision D14 (native mux).** v1 used smux plus a stream-kind byte plus vision padding. v2 replaces
all three with connection-level frames of frame v2, so every carrier uses one frame format (V6).

The TLS application data is a sequence of frames (§6.1). At connection level only these types are
valid: `MUX_STREAM`, `MUX_RESET`, `MUX_WINDOW`, `MUX_MAX_DATA`, `MUX_DATAGRAM`, `PADDING`.

```
MUX_STREAM    payload = stream_id(varint) ‖ flags(u8) ‖ data           data ≤ 16384 bytes
              flags: 0x01 FIN; other bits reserved: sent as 0, ignored on receipt
MUX_RESET     payload = stream_id(varint) ‖ code(varint)                 registries.md §10.2
MUX_WINDOW    payload = stream_id(varint) ‖ increment(varint)
MUX_MAX_DATA  payload = increment(varint)                                connection-level window
MUX_DATAGRAM  payload = one datagram (§6.5); best effort, dropped when the queue is full
```

- Stream IDs: bit 0 is the opener (0 = carrier client, 1 = carrier server); IDs of each opener
  increase by 2 (0, 2, 4, … and 1, 3, 5, …). Stream 0 is the control stream. A stream is opened by its
  first `MUX_STREAM`. A sender MUST open its streams in increasing ID order and announces each new
  stream at once (an empty `MUX_STREAM` is allowed for this). A receiver treats an ID of that opener
  below the highest it has seen, and not currently open, as a finished stream: its data is discarded.
- At most 256 streams open per side, counted per mux connection and per session. An excess stream is
  refused with `MUX_RESET{refused}` (mux) or `CLOSE{refused}` (QUIC); this is not abuse.
- The mux has no unidirectional streams (its IDs carry no direction bit). The 64 incoming unidirectional
  streams of §10 exist only on QUIC carriers; on the mux, one-way traffic uses a bidirectional stream
  that the receiver never writes to.
- `MUX_RESET` ends a stream at once; data received before the reset stays readable (so a `CLOSE`
  frame sent just before it is delivered).
- Unknown frame types at connection level: an IGNORABLE frame (GREASE `0x3e` included) is skipped; a
  frame without IGNORABLE closes the carrier, since there is no stream to close. Neither is abuse.
- Flow control: initial window 262144 bytes per stream and 4194304 per connection. A receiver sends a
  window update when half of a window is consumed. Data beyond the window is a protocol violation.
- The bytes of each stream are themselves a sequence of stream frames (§6.2), exactly as on a QUIC
  stream.

## 6. Frame v2

### 6.1 Stream frame

```
frame = type(varint) ‖ flags(u8) ‖ length(varint) ‖ payload(length bytes)
```

| Flag bit | Name | Meaning |
|---|---|---|
| 0 (0x01) | IGNORABLE | a receiver that does not know `type` skips the frame |
| 1–7 | — | reserved: MUST be 0 when sent; a frame with a reserved bit set is treated as an unknown type |

- `type` values: [registries.md](registries.md) §1, §4. The type space is global for `0x00–0x3f`
  (session level) and per service for `0x40–0x3fff`.
- **Receiving unknown types.** `0x00`, `0x3f`, GREASE `0x3e` and every unassigned value are never
  known types: skipped with IGNORABLE, otherwise `CLOSE{unknown-frame}` on a stream (connection level:
  §5.2; stream 0: §6.2). Unknown types are never abuse.
- **Known types in the wrong place** — a session-level frame on a stream where registries.md §1 does
  not allow it (PING on a service stream, OPEN on stream 0, HS_* after the handshake) — are a protocol
  violation of a known type: `CLOSE{protocol-violation}` (on stream 0: `GOAWAY{protocol-violation}`),
  and they MAY count as abuse.
- **Frames that a feature introduces** (OBSERVED_ADDR, CARD_UPDATE, PUNCH_OFFER, PUNCH_SYNC,
  PATH_BIND and the PATH_* frames) received when the feature was not negotiated are treated as
  unknown types. A PATH_BIND for a session without `path-migration` gets the silent close of §8.2.
- `length` MUST NOT exceed the type's maximum (registries) and never 1048576 (1 MiB). A longer frame
  is `CLOSE{frame-too-large}`. An empty payload is allowed where the type allows it.
- A receiver reads frames with a bounded buffer; it never allocates more than the type's maximum
  before the payload arrives.

### 6.2 Streams

- Stream 0 starts with `HS_HELLO` (a new session) or `PATH_BIND` (a new path of an existing session).
  After the handshake it is the session's **control stream**.
- Every other stream starts with `OPEN`. A stream whose first frame is not OPEN is closed with
  `CLOSE{protocol-violation}`.
- One stream carries one use of one service. A service MAY keep a stream open for many requests
  (dht/2 does).
- Unidirectional streams carry one-way traffic of the same frames (for example chat/2 DELIVER without
  a response).
- A stream other than stream 0 opened before the handshake has completed is reset with
  `CLOSE{protocol-violation}` and its data discarded.
- An unknown session-level frame without IGNORABLE on stream 0 after the handshake is a protocol
  violation (`GOAWAY{unknown-frame}`): frames that a feature introduces are sent only after the feature
  was negotiated (versioning.md §3.3), so a correct peer never sends one.

### 6.3 OPEN, UNSUPPORTED, CLOSE

```
OPEN         payload = service(varint) ‖ version(varint) ‖ flags(u8)
             flags: 0x01 EARLY (the stream was opened in 0-RTT data); others reserved: sent as 0,
             ignored on receipt
UNSUPPORTED  payload = service(varint) ‖ min(varint) ‖ max(varint)      versions the receiver serves; 0,0 = none
CLOSE        payload = code(varint) ‖ reason(UTF-8, ≤ 200 bytes)
```

- OPEN with a `(service, version)` that was not selected in the handshake is answered with UNSUPPORTED
  and the stream ends with a normal FIN (not a reset, which could discard the UNSUPPORTED frame). This
  is not abuse and MUST NOT be scored.
- A selected `(service, version)` is usable in both directions: either side may open streams of it. A
  side that has no handler for a stream the peer opens answers `UNSUPPORTED{service, 0, 0}`.
- CLOSE ends the stream in both directions after the frame. On QUIC the stream is then reset with
  application error code `code`; on the mux, `MUX_RESET` follows. A CLOSE on stream 0 ends that
  **path**, not the session (GOAWAY ends the session).

### 6.4 PADDING

`PADDING` (type 0x01) may appear anywhere a frame may appear, including at connection level of the
mux. Its payload is ignored; senders SHOULD fill it with zero bytes (TLS encrypts it). Padding is a
sender policy governed by the shape profile (§9).

### 6.5 Datagrams

```
datagram = type(varint) ‖ payload            (the length is the datagram length)
```

Types: [registries.md](registries.md) §2. A datagram is never answered with an error. Unknown types are
dropped and counted.

### 6.6 Vectors

[vectors/varint.json](vectors/varint.json) has QUIC varint encodings (all length classes, both
boundaries, and non-minimal encodings that MUST be rejected); [vectors/frames.json](vectors/frames.json)
has encoded frames of every session-level type with a fixed payload.

## 7. Session control

### 7.1 PING, PONG, keepalive

- `PING` carries 8 opaque bytes; the peer answers `PONG` with the same bytes within 1 s of receipt.
- Keepalive: when a path — active or standby (§8.3) — has been idle, a node sends PING on that path's
  stream 0 after `uniform(12, 24)` s of silence. A path with no authenticated traffic for 60 s is dead.
- **Periodic timers** (keepalives, republishing, refreshes, rotations, cache flushes, polling) MUST have
  at least ±20 % uniform jitter (V2). Deadlines and timeouts (the 1 s PONG deadline, the 15 s GOAWAY
  drain, handshake timeouts) are not periodic and are not jittered.

### 7.2 OBSERVED_ADDR and address discovery

- With feature `observed-addr`, the responder includes the initiator's source address in
  `HsResp.observed` and MAY send `OBSERVED_ADDR` when it changes.
  `payload = family(u8: 4 or 6) ‖ address(4 or 16) ‖ port(u16)`.
- **Decision D20 (no plaintext STUN).** A node MUST NOT send plaintext STUN from a socket that carries
  `noise3` or `reality2-quic` traffic (K2). Address discovery uses OBSERVED_ADDR from authenticated
  peers. A node treats an address as its public address only when at least 2 peers in different /16
  (IPv6: /32) networks agree on the IP, and never breaks a tie by any ordering; NAT classification
  needs 3 observations. A divergent IP from one peer is a suspicious observation, not evidence of a
  symmetric NAT. A node MAY use STUN from a separate socket with randomised timing outside guard mode.

### 7.3 GOAWAY

`GoAway { uint32 code = 1; string reason = 2; uint32 retry_after = 3; }`. After sending GOAWAY a node
opens no new streams and closes the session when the streams of **both** sides have ended, or after
15 s.

**One session per pair.** A node keeps at most one live session per (local identity, peer device
ID). When both sides dial each other at the same time, the session whose **initiator has the
smaller device ID** (bytewise, 32 bytes) is kept, and the other one is closed with
`GOAWAY{code = 0 no-error}` (with `retry_after` when the caller has one). Both sides know both
device IDs, so the decision is symmetric and needs no extra message; the losing session drains
under the rule above before it goes away.

## 8. Paths

A **session** ([handshake.md](handshake.md) §9) is identified by its 16-byte session ID `sid` and
holds the session secret and the negotiated features and services. A **path** is one way of carrying
the session's frames:

- a **direct path**: a carrier connection (any L0 suite) whose TLS session is bound to the session;
- a **relay path**: a relay circuit ([services.md](services.md) §6) carrying sealed records.

Streams belong to a path: a QUIC stream exists on one QUIC connection, a mux stream on one byte-stream
carrier. When a path dies, its streams are reset with `path-failed` and services reopen them on
another path; the session, its keys and its peer authentication survive without a new handshake. A
session whose last path has died waits 60 s (the path-dead time) for a PATH_BIND and then ends.

### 8.1 Path IDs

`path_id` is a varint chosen by the side that creates the path: `2k` for paths created by the session
initiator, `2k + 1` for paths created by the responder. The path of the handshake is 0. Either session
role may create paths; the creator is the client of the new carrier connection and sends PATH_BIND on
its stream 0.

### 8.2 PATH_BIND (feature `path-migration`)

A new carrier connection to the same peer is bound to an existing session instead of running a new
handshake. Stream 0 of the new carrier starts with:

```
PATH_BIND     payload = sid(16) ‖ path_id(varint) ‖ mac(32)
k_path        = HKDF-SHA256(ikm = session_secret, salt = h, info = "echofog/session2/path-bind", L = 32)
mac           = HMAC-SHA256(k_path, "echofog/session2/path-mac" ‖ cb(32) ‖ sid(16) ‖ u64(path_id) ‖ u8(role))
PATH_BIND_OK  payload = path_id(varint)
```

- `cb` is the channel binding of the **new** carrier's TLS session ([handshake.md](handshake.md)
  §3.2); `role` is 0 if the sender is the session initiator, 1 otherwise; `session_secret` and `h` are
  from [handshake.md](handshake.md) §9.
- The receiver checks: `sid` known, MAC valid (constant time), `path_id` unused and of the sender's
  parity. On success it answers `PATH_BIND_OK` and the new stream 0 becomes a control stream of the
  session. On any failure it closes the carrier silently (a hidden node reveals nothing to someone who
  does not hold the session secret).
- Because the MAC covers the new channel binding, a captured PATH_BIND cannot be replayed on another
  TLS session.
- On a path in sealed mode (§8.5), PATH_BIND and PATH_BIND_OK are plain frames; records start right
  after PATH_BIND_OK.

### 8.3 Choosing and abandoning paths

- Each side sends on one **active path** at a time (multipath scheduling is the reserved feature
  `multipath`). It prefers direct paths over relay paths and lower RTT among them.
- `PATH_ABANDON{path_id, code}` tells the peer a path will no longer be used.
- **Standby paths.** Every other live path is a standby path. A node keeps at most 2 standby paths per
  session (with `low_power`, none: platforms.md §2); it abandons the oldest beyond that.
- **Liveness.** Every path, active or standby, sends PING when it has been idle for
  `uniform(12, 24)` s (§7.1), so a standby path keeps its NAT bindings and proves it is alive. A path
  with no authenticated traffic for 60 s is dead and is abandoned.
- **Switching.** The active path is **failing** when a PING on it gets no PONG within
  `max(1 s, 3 × RTT)`, or when it carries no authenticated packet for 10 s while the node has data or
  PINGs outstanding on it. A failing active path is replaced by the best standby path at once; it
  stays a standby path until it is dead (60 s) or recovers. Idleness alone never abandons a path.
- **Outstanding data without acknowledgements.** The mux has no acknowledgements, so "data outstanding"
  cannot be read off the wire. A node that has sent data on the active path and received nothing on it
  for 5 s (half of the 10 s above) sends a **probe PING** on that path at once, whatever its idle
  timer says; the PONG deadline above then decides. Without the probe, a one-way sender would
  mark a healthy path as failing. A node that has no data outstanding sends no probe.
- Resuming a stream across paths (the successor of v1 TCP rotation against flow-freezing) is a later
  feature, `stream-resume` (reserved, [registries.md](registries.md) §5).

### 8.4 Sealed records

Where there is no TLS between the two ends that protects the session with post-quantum key exchange —
on relay paths, and on direct paths in sealed mode (§8.5) — the end nodes protect frames with the
Noise transport keys: `Split()` of the handshake gives `k_i2r` (initiator → responder) and `k_r2i`
([handshake.md](handshake.md) §9).

```
sealed  = n(u64) ‖ ChaCha20-Poly1305(k_dir, nonce = 0^4 ‖ u64le(n), aad, plaintext)
aad     = sid(16) ‖ u64(n) ‖ u8(class) ‖ varint(path_id) ‖ varint(pipe)
          class 1: an ordered record; pipe = the QUIC stream ID (sealed mode on QUIC), or 0 (a stream
                   circuit, or a mux carrier in sealed mode: the whole carrier is one pipe)
          class 2: a SEALED datagram; pipe = 0
record  = u16(1 + len(body)) ‖ kind(u8) ‖ body
          kind 0: body = plaintext frames      (only before the handshake completes, only HS_* frames)
          kind 1: body = sealed
          other kinds: reserved; a record with an unknown kind ends the path
```

- **One counter `n` per direction per session**, shared by all paths, all pipes, records and
  datagrams alike. A sender never reuses a value (with two relay paths and per-path counters the same
  key would reuse nonces) and stops at `2^60` (the session must then be re-established).
- **Replay protection for ordered records.** Records of one pipe arrive in order, and since the sender
  draws every `n` from one growing counter, the `n` of successive records of a pipe is strictly
  increasing. The receiver keeps, per pipe, the last accepted `n` and rejects a record whose `n` is not
  larger. There is no window: a pipe the application reads slowly can never lose records. The AAD binds
  each record to its path and pipe, so a record cannot be moved into another pipe.
- **Replay protection for datagrams.** `SEALED` datagrams (on relay datagram circuits and in sealed
  mode) use a sliding window of 4096 values per direction per session, over the datagram `n` values
  only.
- **Stream circuits** carry records; the plaintext of a sealed record is a sequence of connection-level
  mux frames (§5.2). The `u16` length value (`1 + len(body)`, the kind byte and the body, without the
  2-byte prefix itself) is at most 16384 + 64. The switch from kind 0 to kind 1 is explicit: the first
  record after the handshake has kind 1, and a kind-0 record after that ends the path.
- A `SEALED` datagram whose plaintext is again a `SEALED` datagram, a `SEALED` datagram on a path that
  is not in sealed mode and not a relay path, and a plaintext datagram on a sealed path are dropped and
  counted.
- **Relay paths are always sealed for datagrams.** Every datagram on a relay path is a `SEALED`
  datagram (class 2, bound to that path by `path_id`), whether or not the session has a sealed direct
  path; a plaintext datagram on a relay path is dropped and counted. A direct path that is not in
  sealed mode carries plaintext datagrams only.
- **Datagram circuits** carry one `SEALED` datagram (type 0x02) per relay datagram; its payload is
  `sealed` and its plaintext is one inner datagram (§6.5).
- A relayed session's handshake runs inside the first stream circuit (kind-0 records), with the
  circuit channel binding of [handshake.md](handshake.md) §3.2.

### 8.5 Sealed mode on direct paths

A direct path whose carrier TLS negotiated a classical group (a REALITY path whose cover does not use
`X25519MLKEM768`, §3.4) runs in **sealed mode**. After the handshake on stream 0, the bytes of every
stream on that path, stream 0 included, are records of §8.4 (kind 1), and every datagram is a `SEALED`
datagram. On QUIC each QUIC stream is a pipe of its own; on a byte-stream carrier the records wrap the
whole connection-level frame sequence (MUX_* frames included), so the carrier is one pipe, as a stream
circuit is.

**The switch point on a byte-stream carrier.** There is no marker in the bytes: the first record
starts exactly where the last byte of the handshake (or of `PATH_BIND_OK` on a bound path) ended. So:
the handshake reader reads stream 0 one frame at a time and never reads ahead (it must not buffer
bytes beyond the frame it returns); it leaves the sealing switch to the session layer, which calls
it once, after the handshake completes (after `PATH_BIND_OK` on a bound path), before any other
read of the carrier. Every byte after that point is a record; a plaintext frame there ends the path.
Both sides know the point because both know when the last handshake frame was sent and received.
Both sides know the
negotiated group, so no signalling is needed. Paths with `X25519MLKEM768` are never in sealed mode.

## 9. Padding and shape profiles

A **shape profile** is data, not code: it tells a sender how to size and time what it sends, and
tells peers what the responder's network expects. Profiles never change a cryptographic format;
receivers MUST accept any length and timing within the limits of §10. A card address carries a profile
by built-in ID or in full (`Addr.params`).

```
message ShapeProfile {
  uint32   id            = 1;   // built-in id (registries.md §6.3), 0 = custom
  Range    init_len      = 2;   // noise3: total Init datagram length
  Range    pad           = 3;   // extra padding per data packet / per TLS record, uniform
  repeated uint32 buckets = 4;  // if set: pad each packet or record up to the next bucket size
  Range    pad_budget    = 5;   // TLS carriers: bytes per stream direction during which padding applies
  Junk     junk          = 6;   // noise3: junk datagrams before the first Init
  repeated Decoy decoys  = 7;   // noise3: decoy datagrams before the first Init
  Range    keepalive_s   = 8;   // PING interval when idle
  Range    idle_pad_ms   = 9;   // with feature idle-padding: gap between padding-only packets
}
message Range { uint32 min = 1; uint32 max = 2; }          // uniform over [min, max]
message Junk  { Range count = 1; Range len = 2; Range gap_ms = 3; }
message Decoy {
  uint32 kind     = 1;          // 1 raw template, 2 DNS query, 3 QUIC-Initial-shaped random
  bytes  template = 2;          // kind 1: bytes; 0xff…ff runs are replaced by random bytes
  string qname    = 3;          // kind 2: the query name
}
```

Built-in profiles:

| Field | `default-udp` (1) | `default-tls` (2) |
|---|---|---|
| `init_len` | [1280, 1420] | — |
| `pad` | [0, 48] per packet, never beyond the path MTU | [0, 256] per record within the budget |
| `buckets` | — | 512, 1024, 1460, 2920, 4096 |
| `pad_budget` | — | [4096, 16384], drawn per stream direction |
| `junk` | count [0, 2], len [32, 256], gap [5, 30] ms | — |
| `decoys` | none | — |
| `keepalive_s` | [12, 24] | [12, 24] |
| `idle_pad_ms` | [500, 4000] | [500, 4000] |

The initiator SHOULD use the responder's profile for what it sends to that address, so both directions
match what the address's network is expected to look like.

**Buckets and pad.** When `buckets` is set, `pad` is not used: each packet or TLS record is padded up to
the next bucket size and nothing more. `pad_budget` is counted per stream direction, not per sender. The
`default-tls` row above lists both `pad` and `buckets`; which of the two it keeps is an open question for
the architect (see SPEC-FIX-1106b report), so the row is unchanged here.

## 10. Limits and timeouts

| Item | Value |
|---|---|
| live masking keys per listener (noise3 and REALITY v2) | ≤ 8 |
| stranger first-packet budget (noise3) | 50/s per source IP, 2000/s per listener |
| noise3 Init timestamp window / replay cache | ±300 s / 900 s |
| REALITY token timestamp window / replay cache | ±300 s / 900 s |
| noise3 pending flows | 64 per source IP, 4096 total, random eviction |
| noise3 packet-number reconstruction / replay bitmap | ±2^31 / 1024 behind |
| noise3 dead-flow restart | no authenticated packet for `uniform(12, 18)` s |
| reality2 pending hellos | 256, 8 KiB per source, 500 ms; beyond: to the cover |
| reality2 cover forwarding sockets | 1024, idle > 30 s evicted only |
| reality2-tcp pre-token handshakes | 256, first record ≤ 16 KiB within 10 s |
| carrier dial timeout | QUIC 4 s, TCP 6 s, ws2 8 s |
| frame payload | per type; never > 1 MiB |
| mux stream data per frame | ≤ 16384 |
| mux windows | 256 KiB per stream, 4 MiB per connection |
| streams per session per side | 256 bidirectional, 64 incoming unidirectional (QUIC only: the limit a side grants its peer) |
| idle keepalive / path dead | `uniform(12, 24)` s / 60 s |
| sealed replay protection | ordered records: strictly growing `n` per pipe; datagrams: window of 4096 per direction per session |
| standby paths | ≤ 2 per session; keepalive like the active path; dead after 60 s |
| session without a path | ends after 60 s |
| periodic timer jitter | ≥ ±20 % |
