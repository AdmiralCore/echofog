# L1 handshake

v2 authenticates nodes with a Noise handshake (Noise Protocol Framework, revision 34) extended with
the `hfs` modifier (Noise HFS: hybrid forward secrecy with a KEM) using X25519 and ML-KEM-768.

| Suite ID | Protocol name | Used when |
|---|---|---|
| 0x0001 | `EchoFog/2/Noise_XKhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s` | the initiator holds the responder's card (its handshake static key) |
| 0x0002 | `EchoFog/2/Noise_XXhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s` | bootstrap: only an address is known |

The handshake runs on stream 0 of a carrier connection ([wire.md](wire.md) §5–§6), or inside a relay
stream circuit. Test vectors: [vectors/handshake.json](vectors/handshake.json).

## 1. Properties (summary)

- Mutual authentication of the node identities (Ed25519 keys) through their signed cards, which bind
  each node's X25519 handshake static key `hs`.
- Hybrid key agreement: the session keys depend on X25519 (`es`, `ee`, `se`) **and** ML-KEM-768
  (`ekem1`). An adversary who later breaks X25519 (A5) but not ML-KEM learns neither the initiator's
  identity (sent after `ekem1`) nor the session data protected by the Noise keys.
- Channel binding: the carrier's TLS session is part of the prologue, so a man in the middle who
  terminates TLS on both sides makes the handshake fail (§3.2) — whenever the initiator knows whom it
  is calling (XK, or XX with an expected node ID). An XX handshake to a bare address authenticates
  nothing about the responder (§4.2).
- Key binding: the expected responder node ID is in the prologue, so a card that copies another
  node's `hs` key cannot make an initiator believe it talks to the copier (§3.3).
- Downgrade resistance: the protocol name, the offered suite list, the features and the services are
  all in the transcript.
- Identity hiding: XK — the responder reveals nothing that the initiator did not already have (its
  card); the initiator's identity is sent in message 3, encrypted, after the responder proved itself.
- Hidden nodes: the responder answers nothing before a valid invitation proof (§5).

**Decision D3 (protocol name).** The full suite string, including the `EchoFog/2/` prefix, is the Noise
`protocol_name` passed to `InitializeSymmetric`. It is therefore the first input of the handshake hash,
as the design requires, and separates EchoFog handshakes from every other Noise use. Implementations
must allow a custom protocol name (the Noise framework permits any name; some libraries build it from
parts and need a small change).

## 2. Cryptographic functions

| Noise function | Definition |
|---|---|
| `DH(kp, pub)` | X25519 (RFC 7748); an all-zero output MUST abort the handshake |
| `GENERATE_KEYPAIR()` | X25519 key pair from 32 random bytes |
| `KEM_GENERATE()` | ML-KEM-768 key pair (FIPS 203) from a random 64-byte seed `d ‖ z` |
| `KEM_ENCAP(pk)` | ML-KEM-768 encapsulation → `(ct: 1088 bytes, ss: 32 bytes)` |
| `KEM_DECAP(sk, ct)` | ML-KEM-768 decapsulation → `ss` |
| `ENCRYPT(k, n, ad, p)` | ChaCha20-Poly1305 (RFC 8439), nonce = 32 zero bits ‖ `u64le(n)` |
| `HASH` | BLAKE2s-256; `HASHLEN = 32`, `BLOCKLEN = 64` |
| `HMAC-HASH` | HMAC with BLAKE2s-256 (RFC 2104) |
| `HKDF(ck, ikm, n)` | Noise §4.3: `t = HMAC-HASH(ck, ikm)`; `o1 = HMAC-HASH(t, 0x01)`; `o2 = HMAC-HASH(t, o1 ‖ 0x02)`; `o3 = HMAC-HASH(t, o2 ‖ 0x03)` |

`SymmetricState`, `CipherState`, `MixKey`, `MixHash`, `EncryptAndHash`, `DecryptAndHash` and `Split`
are exactly those of Noise revision 34 §5. ML-KEM-768 encapsulation keys MUST pass the FIPS 203
encapsulation-key check before use.

### 2.1 The hfs tokens

As in Noise HFS:

- `e1` — the sender runs `KEM_GENERATE()` and appends `EncryptAndHash(e1.public)` (1184 bytes, plus 16
  if a key is set). The receiver reads `re1 = DecryptAndHash(…)` and checks it.
- `ekem1` — the sender computes `(ct, ss) = KEM_ENCAP(re1)`, appends `EncryptAndHash(ct)` and calls
  `MixKey(ss)`. The receiver reads `ct = DecryptAndHash(…)`, computes `ss = KEM_DECAP(e1.secret, ct)`
  and calls `MixKey(ss)`.

The KEM key pair `e1` is ephemeral: generated per handshake and erased after `ekem1` is processed.

## 3. Before the first message

### 3.1 HS_HELLO

The initiator sends, on stream 0, `HS_HELLO` immediately followed by `HS_MSG` (message 1) in the same
flight.

```
HS_HELLO payload:
  n          u8            number of offered suites, 1..8
  offered    u16 × n       suite IDs, in the initiator's preference order
  chosen     u16           the suite of the following HS_MSG; MUST be in `offered`
  flags      u8            0x01: a puzzle solution follows
                           0x02: the prologue binds an expected responder node ID (§3.3)
                           other bits: reserved, sent as 0, ignored on receipt
  [cookie    16 bytes      from HS_RETRY (§6)
   nonce     u64]          puzzle solution
```

**Decision D4 (suite selection without a round trip).** Noise needs the protocol name before message 1,
so the initiator chooses.

- With a card, `offered` MUST contain exactly the suites that are both supported by the initiator and
  listed in `l1_suites` of the initiator's copy of the responder's card, in the initiator's preference
  order; `chosen` is the first. Without a card, `offered = chosen = XX` (plus GREASE, §3.4).
- The responder accepts only if `chosen` is its **own most preferred** suite in `offered ∩ supported`.
  Because `offered` is limited to the card the initiator holds, and a node keeps supporting every suite
  listed in any of its unexpired cards, a stale card leads to an older suite that still works: hidden
  nodes can add a suite without breaking invitations already issued.
- Otherwise a public node replies `REJECT{reason = suite, detail}` where `detail` lists its suite IDs
  as 4-digit lowercase hex values separated by single commas (`0001,0002`), and closes the stream;
  the initiator may retry once. On retry it MUST send its full original `offered` list again (a forged
  REJECT before Noise cannot narrow it), choosing the best suite of the intersection.
- A hidden node closes silently, with the uniform delay of §5.

Because `HS_HELLO` is in the prologue, an attacker who removes suites from `offered` causes a
transcript mismatch and the handshake fails; because `offered` is fixed by a signed card, the
initiator's own choice cannot be pushed down either.

### 3.4 GREASE in the handshake

To keep the "unknown is ignored" rules exercised, an initiator SHOULD add one GREASE value of the form
`0xXAXA` (X in 0..e) to `offered` (never as `chosen`), to `HsInit.features`, and as a service ID with
version 1 to `HsInit.services`. Responders MUST ignore GREASE values. Lists stay sorted with the
GREASE value in its numeric place.

### 3.2 Channel binding

`cb` (32 bytes) binds the handshake to the transport it runs on:

| Transport | `cb` |
|---|---|
| any carrier (inner TLS 1.3, [wire.md](wire.md) §5.1) | TLS exporter (RFC 8446 §7.5), label `EXPORTER-echofog/hs2/channel`, empty context, length 32 |
| relay stream circuit | `SHA-256("echofog/hs2/circuit" ‖ relay_node_id(32) ‖ u64(cid))`, where `cid` is the single circuit ID that the relay assigns and reports to both ends (services.md §6) |

**Decision D5 (where confidentiality lives, revised).** On a direct path whose inner TLS negotiated
`X25519MLKEM768`, the session's frames are protected by that TLS, and Noise authenticates the TLS
session through `cb`; frames are not encrypted a second time (a third encryption layer costs CPU, which
v1 already suffers from). Otherwise frames are sealed with the hybrid Noise transport keys
([wire.md](wire.md) §8.4–§8.5):

- noise3 paths: the inner TLS MUST use `X25519MLKEM768` (nothing observable depends on it);
- REALITY paths: the group mirrors the cover so the ServerHello matches it; with a classical cover
  group the path runs in sealed mode, so bulk data keeps post-quantum confidentiality either way;
- relay paths: always sealed;
- TLS/QUIC early data MUST NOT be used (its exporter is not the one bound here).

### 3.3 Prologue

```
prologue = "echofog/hs2/prologue" ‖ cb(32) ‖ rid(32) ‖ u8(len(hello)) ‖ hello
```

- `hello` is the exact `HS_HELLO` payload that precedes this message 1 on the stream.
- `rid` is the expected responder node ID when `hello.flags & 0x02` is set, else 32 zero bytes. The
  initiator MUST set the flag and `rid` in XK (it knows the card) and in XX whenever it expects a
  node ID. The responder fills `rid` with **its own** node ID when the flag is set. A card that copies
  another node's `hs_key` (unknown key-share attack) therefore fails: the real owner of `hs` answers
  with its own node ID in the prologue and the transcript no longer matches.

## 4. Messages

Notation: `→` initiator to responder, `←` back. `s`/`rs`: local/remote static X25519 (`hs`);
`e`/`re`: ephemeral X25519; `e1`/`re1`: ephemeral ML-KEM-768. `P1`, `P2`, `P3` are the payloads of §7,
encoded as protobuf, each passed to `EncryptAndHash` after the message's tokens.

Each Noise message is sent as one `HS_MSG` frame whose payload is the message bytes. A Noise message
MUST NOT exceed 16384 bytes.

### 4.1 XKhfs

```
XKhfs:
  <- s
  ...
  -> e, e1, es
  <- e, ee, ekem1
  -> s, se
```

Initialisation, both sides: `InitializeSymmetric(protocol_name)`; `MixHash(prologue)`;
`MixHash(rs)` (the responder's `hs` public key, from the initiator's copy of the card; the responder
uses its own). During an `hs` rotation the responder holds a current and a previous key (§7.1) and
tries both: one extra X25519 per message 1, only while the previous key is live.

| Step | Initiator writes | Responder reads |
|---|---|---|
| 1.1 `e` | `e = GENERATE_KEYPAIR()`; append `e.pub`; `MixHash(e.pub)` | `re` = 32 bytes; `MixHash(re)` |
| 1.2 `e1` | `e1 = KEM_GENERATE()`; append `EncryptAndHash(e1.pub)` (no key yet: plaintext, hashed) | `re1 = DecryptAndHash(1184 bytes)` |
| 1.3 `es` | `MixKey(DH(e, rs))` | `MixKey(DH(s, re))` |
| 1.4 payload | append `EncryptAndHash(P1)` | `P1 = DecryptAndHash(rest)`; failure → §11 |
| 2.1 `e` | — | `e = GENERATE_KEYPAIR()`; append `e.pub`; `MixHash(e.pub)` |
| 2.2 `ee` | `MixKey(DH(e, re))` | `MixKey(DH(e, re))` |
| 2.3 `ekem1` | `ct = DecryptAndHash(1088 + 16)`; `MixKey(KEM_DECAP(e1, ct))` | `(ct, ss) = KEM_ENCAP(re1)`; append `EncryptAndHash(ct)`; `MixKey(ss)` |
| 2.4 payload | `P2 = DecryptAndHash(rest)` | append `EncryptAndHash(P2)` |
| 3.1 `s` | append `EncryptAndHash(s.pub)` (32 + 16) | `rs = DecryptAndHash(48 bytes)` |
| 3.2 `se` | `MixKey(DH(s, re))` | `MixKey(DH(e, rs))` |
| 3.3 payload | append `EncryptAndHash(P3)` | `P3 = DecryptAndHash(rest)` |

Sizes without payload: message 1 = 32 + 1184 + 16 = 1232 bytes; message 2 = 32 + 1104 + 16 = 1152
bytes; message 3 = 48 + 16 = 64 bytes.

### 4.2 XXhfs

```
XXhfs:
  -> e, e1
  <- e, ee, ekem1, s, es
  -> s, se
```

Initialisation: `InitializeSymmetric(protocol_name)`; `MixHash(prologue)`; no pre-message.

| Step | Initiator | Responder |
|---|---|---|
| 1 | `e`, `e1` as in XK 1.1–1.2; append `EncryptAndHash(P1)` (plaintext: no key) | read; `P1` in clear |
| 2 | read `re`; `ee`; `ekem1`; `rs = DecryptAndHash(48)`; `MixKey(DH(e, rs))`; `P2` | `e`; `ee`; `ekem1`; append `EncryptAndHash(s.pub)`; `MixKey(DH(s, re))`; `EncryptAndHash(P2)` |
| 3 | as XK message 3 | as XK message 3 |

In XX, `P1` travels without Noise encryption (inside the carrier TLS only) and is not authenticated
until message 2. Therefore `P1` in XX MUST NOT contain `access` or `early`, and SHOULD contain only
`proto_ver`, `features`, `services` and `time`.

**Decision D22 (XX and hidden nodes).** XX reveals the responder's static key and card to anyone who
sends message 1. A hidden node MUST NOT accept XX: it closes the stream silently, with the uniform
delay of §5. Servers and public nodes accept XX only on addresses they list for bootstrap and SHOULD
require a puzzle for it under load.

**XX bootstrap rules.** XX authenticates the responder only if the initiator knows whom to expect:

- Bootstrap lists MUST give a node ID (better the whole card) for every entry. With an expected node ID,
  the initiator sets `rid` (§3.3), checks `SHA-256(card signer akey) = rid` in `P2` before message 3,
  and only then sends its card.
- With a bare address and no expected ID, the initiator MUST use a **throwaway identity** (a fresh
  Ed25519 key and a minimal card, never reused), and treats every card and address learned in that
  session as an unverified candidate; it MUST NOT use such a session for anything but finding
  candidates to verify through other, authenticated sessions.

## 5. Hidden nodes and invitations (K5)

An invitation link carries a 16-byte **access secret** `S` together with the invitation card
([objects.md](objects.md) §10). In XK, `P1.access` proves possession of `S`:

```
pass_id = HMAC-SHA256(S, "echofog/hs2/pass-id")[0:8]
proof   = HMAC-SHA256(S, "echofog/hs2/access-proof" ‖ h1)
```

`h1` is the handshake hash immediately before `EncryptAndHash(P1)` (after step 1.3). It depends on the
fresh ephemeral `e`, so a proof cannot be replayed into another handshake. `pass_id` lets the responder
find the secret with one index lookup instead of trying every pass (v1 tried up to 10 000 HMACs under
a write lock).

The responder MUST process message 1 in this order and MUST NOT send any byte on the stream before
step 5 succeeds:

1. `HS_HELLO` well-formed; suite rule of §3.1 (hidden: XK only).
2. Puzzle (§6), public nodes only: if one is demanded, the cookie and the solution are checked here,
   before any public-key operation; without a valid solution the answer is `HS_RETRY`.
3. Message 1 tokens and `P1` decryption.
4. If the node is hidden: `P1.access` present, `pass_id` found, the pass not revoked or expired, and
   `proof` equal in constant time.
5. Only now: early-data processing (§10), `P2`, message 2.

**Uniform failure.** A hidden node treats every failure of steps 1, 3 and 4 the same way: it keeps the
carrier open, waits until `uniform(0.5, 3)` s have passed since message 1 arrived, and closes the
carrier without a message. The time to close does not reveal which check failed, so a holder of an old
card and a live masking key cannot confirm the node's identity at that address.

**Passes after the first session.** An invitation pass is for the first entry. After the first
complete session the hidden node gives the contact, over that session, a **personal pass** (a new
16-byte secret, no use limit, expiry ≤ 1 year, renewed in later sessions) and a peer-scope card with
the node's contacts masking key ([objects.md](objects.md) §10.3). Later connections present the personal
pass in `P1.access`. Revoking a contact deletes its personal pass; revoking an unredeemed invitation
deletes the invitation pass and, if no other unredeemed invitation uses it, the invitation's masking
key.

The masking layer already gates hidden nodes: their noise3/REALITY keys are per invitation, so a
stranger never reaches the handshake at all. The access proof is the second gate, which also covers
leaked or revoked masking keys.

The initiator also protects itself: it sends its identity (message 3) only after message 2 decrypted
(which proves that the responder holds the `hs` key from the card) and after checking `P2` (no
`reject`, acceptable versions). An entity that merely answers on the address learns nothing about who
is calling.

## 6. Puzzle (HS_RETRY)

A public responder under load, or for XX from strangers, answers `HS_HELLO` + message 1 with
`HS_RETRY` instead of message 2, without processing message 1 (step 2 of §5):

```
HS_RETRY payload = cookie(16) ‖ difficulty(u8)
cookie           = HMAC-SHA256(R, "echofog/hs2/cookie" ‖ cb(32) ‖ u64(minute))[0:16]
```

`R` is a 32-byte responder secret rotated daily; `minute = floor(unix / 60)`. The initiator finds
`nonce` such that

```
SHA-256("echofog/hs2/puzzle" ‖ cookie(16) ‖ cb(32) ‖ u64(nonce))
```

has at least `difficulty` leading zero bits, then sends, on the same stream 0 of the same carrier
connection, a new `HS_HELLO` with the solution and a new message 1 (fresh ephemerals). The responder
accepts cookies of the current and the previous minute; it is stateless until then. The puzzle bounds
Noise work; the carrier's TLS handshake before it is bounded by the per-source limits of §11 and, on
QUIC, by address validation (Retry) after the L0 check. Difficulty is 0–24; an initiator MUST refuse more than 24. The default
policy is that of v1 (0 below 25 handshakes/s, 16 below 100, 20 below 400, else 22), raised by 2 bits
for each per-source limit of §11 that the source exceeds.

## 7. Payloads and negotiation

```
message HsInit {                        // P1
  uint32 proto_ver = 1;                 // 200
  repeated uint32 features = 2;         // offered feature IDs, ascending, unique (packed)
  repeated ServiceOffer services = 3;   // ascending by id
  Access access = 4;                    // hidden responders only; XK only
  uint64 known_card_seq = 5;            // seq of the responder card the initiator holds (XK)
  uint64 time = 6;                      // initiator clock, unix seconds
  bytes early = 7;                      // 0-RTT stream (§10); XK only
  bytes pad = 15;                       // random length, zero bytes
}
message ServiceOffer { uint32 id = 1; repeated uint32 versions = 2; }   // versions ascending
message Access { bytes pass_id = 1; bytes proof = 2; }                 // 8 and 32 bytes

message HsResp {                        // P2
  uint32 proto_ver = 1;
  repeated uint32 features = 2;         // selected: offered ∩ supported, GREASE removed
  repeated ServiceChoice services = 3;  // one version per service the responder serves to this peer
  bytes card = 4;                       // responder card: REQUIRED in XX; in XK if newer than known_card_seq
  Reject reject = 5;                    // set: the handshake ends after message 2
  uint32 early_status = 6;              // 0 no early data, 1 processed, 2 ignored (resend after msg 3)
  bytes early = 7;                      // response frames of the 0-RTT stream
  bytes observed = 8;                   // family(1) ‖ addr(4|16) ‖ port(2), feature observed-addr
  uint64 time = 9;
  bytes pad = 15;
}
message ServiceChoice { uint32 id = 1; uint32 version = 2; }

message HsFinish {                      // P3
  bytes card = 1;                       // initiator card, REQUIRED
  bytes pad = 15;
}

message Reject {
  uint32 reason = 1;                    // registries.md §10.1
  uint32 min = 2;
  uint32 max = 3;
  string detail = 4;                    // ≤ 200 bytes UTF-8
  uint32 retry_after = 5;               // seconds
}
```

Rules:

- `proto_ver < 200` → `Reject{version, min = 200, max = 200}`. A `proto_ver` higher than the
  receiver's own is accepted; both sides then use the receiver's own behaviour (minor versions add only
  optional behaviour, a new major version has its own suite).
- `|time − now| > 600 s` → `Reject{clock-skew}`. Both `HsInit.time` and `HsResp.time` are always sent;
  each side keeps the median offset to its last 5 authenticated peers and applies it to the timestamps
  it puts into L0 first packets (wire.md §2.4, §3.2), whose window is ±300 s. A node with no recent
  peer SHOULD check its clock against the `Date` header of an HTTPS response before dialing, and
  shows the user a clock warning when its offset exceeds 60 s.
- `HsInit.features` and `HsInit.services` are sorted ascending, GREASE values in their numeric place.
- The initiator MUST abort if `HsResp.features` contains a feature it did not offer, or a service
  choice is not among its offered versions.
- **Card checks** (both directions): the card is a valid `echofog/card/2` object
  ([objects.md](objects.md) §3), not expired, `card.hs_key` equals the Noise remote static key
  (`rs`), and, if the initiator expects a node ID, `SHA-256(signer akey)` equals it. The responder
  checks the initiator card after message 3; the initiator checks a card in `P2` before sending
  message 3.
- **hs_key ↔ node ID binding.** Each node remembers which node ID each `hs` key it has seen in a
  valid card belongs to. A card whose `hs_key` (or `prev_hs`) is already bound to a different signer is
  rejected and counted as abuse against the node that supplied it.
- After message 3 the responder applies admission (bans, per-node and per-address limits, guard
  policy) to the now-authenticated node ID. A refusal is `REJECT` on the control stream, then close.

### 7.1 Rotating the handshake key

A card carries the current `hs_key` and optionally `prev_hs {key, not_after}`. When a node rotates
`hs`, it publishes a card with the new key and the old one in `prev_hs`, with `not_after` equal to the
latest `not_after` of any card (any scope) that listed the old key as current. Until then it accepts
message 1 for either key. Initiators always use `hs_key`.

## 8. Rejections

- In `P2.reject` (the responder has decrypted message 1, and for hidden nodes the access proof was
  valid): version, clock skew, overload, policy. The initiator does not send message 3.
- As a `REJECT` frame on stream 0 before Noise (public nodes only): `suite`, `overloaded`.
- As a `REJECT` frame after message 3: `banned`, `rate-limited`, `card-invalid`, `policy`.

## 9. After the handshake

```
(k_i2r, k_r2i)  = Split()                                      Noise §5.2: HKDF(ck, empty, 2)
session_secret  = HKDF(ck, "echofog/hs2/session-secret", 2).o1  Noise HKDF, label as ikm
h               = the final handshake hash
sid             = HKDF-SHA256(ikm = session_secret, salt = h, info = "echofog/session2/id", L = 16)
```

- `k_i2r`/`k_r2i` are used only for sealed records and datagrams on relay paths
  ([wire.md](wire.md) §8.4).
- `session_secret` keys path binding ([wire.md](wire.md) §8.2). The derivation from `ck` uses a
  non-empty input distinct from `Split`, so it is independent of the transport keys.
- `sid` identifies the session to services that bind requests to it (mailbox FETCH and ACK).
- Stream 0 becomes the control stream. All ephemeral secrets, `ck` and the KEM key pair are erased.

## 10. 0-RTT data

The XK message 1 payload is encrypted to the responder's static key; it can carry one request, so a
DHT lookup to a known node completes in the handshake round trip.

- Only in XK, and only when the initiator offers feature `early-data`. The responder processes it only
  if it supports `early-data` too.
- `P1.early` holds the frames of **one** stream: `OPEN` with flag `EARLY`, followed by service frames.
  At most 1200 bytes. Only service operations marked 0-RTT safe ([registries.md](registries.md) §3) are
  allowed; anything else makes the responder ignore the early data (`early_status = 2`).
- The responder processes early data only if `|P1.time − now| ≤ 60 s` (after applying its learned
  offset) and `e.pub` is not in its 0-RTT replay cache (kept 180 s, keyed by the initiator's ephemeral
  key). A hidden node processes it only after the access proof.
- The response goes into `P2.early` (≤ 8192 bytes), encrypted after `ee` and `ekem1` and so
  forward-secret. `early_status = 2` tells the initiator to resend the request on a normal stream.
- Properties the application must accept: early data is not forward-secret against a later theft of
  the responder's `hs` key, can be replayed within the window (the replay cache bounds it to once per
  responder), and is not authenticated as coming from the initiator's identity. That is why only public,
  idempotent queries qualify.

## 11. Errors, limits, timeouts

| Item | Value |
|---|---|
| wait for each handshake message | 5 s |
| whole handshake | 10 s |
| pre-authentication handshakes | 64 per source IP, 256 per /24 (IPv6 /48), 1024 per node |
| above a per-source limit | public nodes: puzzle difficulty +2 bits per exceeded limit; hidden nodes: queued, then the uniform silent close |
| pre-authentication bans | none: a source IP may be a CGNAT shared by thousands of users; bans apply only to authenticated node IDs |
| HS_HELLO payload | ≤ 64 bytes |
| Noise message | ≤ 16384 bytes |
| card in P2/P3 | ≤ 4096 bytes |
| retries after REJECT{suite} | 1 |

Any decryption failure, malformed token, all-zero DH output, or invalid ML-KEM key ends the handshake
by closing the carrier connection without a message. The responder never tells the initiator which
check failed before message 2.

## 12. Vectors

[vectors/handshake.json](vectors/handshake.json) contains, computed by the reference generator:

- the initial `h` and `ck` for both protocol names, and `h` after `MixHash(prologue)` and the XK
  pre-message for a fixed prologue and static key;
- the encoding of `HS_HELLO` (with and without a puzzle solution), of the prologue, and of `HsInit`,
  `HsResp`, `HsFinish` payloads;
- `pass_id` and `proof` for a fixed secret and `h1`; a puzzle hash and a solution for difficulty 12;
- the post-handshake derivations (`session_secret`, `sid`) from fixed `ck` and `h`.

**Decision D15 (handshake transcript vectors).** Full message-by-message transcripts of XKhfs and
XXhfs (fixed ephemerals, ML-KEM seeds and encapsulation randomness) are **to be produced with the
implementation in step 2**. No existing Noise library implements the `hfs` tokens; values computed only
by a new, unreviewed implementation would not be reliable reference values. The vector file states the
inputs to use. **Exit criterion of step 2:** two independently written implementations (for example Go
and Rust or Python, by different authors) produce byte-identical transcripts from these inputs; until
then this specification is not final for L1.
