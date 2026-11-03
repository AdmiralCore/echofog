# EchoFog protocol v2

Status: **draft specification, step 1 of the v2 plan** (document only; no implementation yet).
Protocol version number: **200**. `MinProtocolVersion` = 200.

This directory specifies the second generation of the EchoFog wire protocol: how nodes hide their
traffic, authenticate each other, open services, publish signed descriptions of themselves, and
evolve without breaking. It replaces protocol v1 (0.2.x, numbers 101/102) entirely. v2 does **not**
interoperate with v1: there are no deployed v1 users to keep, so v2 is a clean break.

The key words MUST, MUST NOT, REQUIRED, SHALL, SHOULD, SHOULD NOT, RECOMMENDED, MAY and OPTIONAL
are to be interpreted as described in RFC 2119 and RFC 8174 when, and only when, they appear in
capitals.

## Documents

| File | Contents |
|---|---|
| [README.md](README.md) | overview, threat model, goals, non-goals, how the layers fit |
| [versioning.md](versioning.md) | the eight versioned layers (L0–L7), negotiation rules, release manifest, dual stack for later versions |
| [wire.md](wire.md) | L0 masking suites (noise3, REALITY v2, ws2), frame v2, byte-stream mux, paths, padding and shape profiles |
| [handshake.md](handshake.md) | L1: Noise XKhfs / XXhfs with X25519 + ML-KEM-768, every message, payload and KDF step, 0-RTT, hidden nodes |
| [objects.md](objects.md) | L5: signed object envelope, node card, exit offer, device list, prekeys, release manifest, links v2 |
| [registries.md](registries.md) | every number and string: frame and datagram types, services, features, suites, labels, algorithm tags, DHT record kinds, error codes |
| [dht.md](dht.md) | DHT v2: keyspace, records, storage, lookup, routing-table rules |
| [services.md](services.md) | L3: dht, pex, chat, mailbox, relay, exit, files, call |
| [security.md](security.md) | claimed properties, the attack each defeats, mapping to v1 weaknesses, residual risks |
| [platforms.md](platforms.md) | gomobile builds (Android, iOS), non-portable pieces and their fallbacks, capability profile and flags, mobile testing |
| [CHANGES-from-v1.md](CHANGES-from-v1.md) | what changed and why, tied to weakness IDs |
| [vectors/](vectors/) | JSON test vectors and their reference generator (`vectors/gen`) |

Weakness IDs (K1–K10, V1–V7, P1–P11, N1, C12) refer to the v1 weakness review that motivated this
specification. They are used only as cross-references; each one is restated briefly where it
matters.

## 1. What EchoFog is

EchoFog is a peer-to-peer network without company servers. The same nodes carry a messenger
(personal chats, groups, files), voice and video calls (including large group calls relayed
through a tree of participants), and a market of VPN exits paid in TON. Nodes run on phones and
desktops behind NAT and on VPS servers; many users are in countries that actively censor the
Internet.

## 2. Threat model

The protocol is designed against these adversaries. Each property claimed in
[security.md](security.md) names the adversary it holds against.

| ID | Adversary | Capabilities |
|---|---|---|
| A1 | **On-path censor** (national DPI: GFW, TSPU, Iranian filters) | sees and records all packets of a user; can drop, delay, inject and replay; runs ML classifiers on flow metadata; blocks by IP, port, SNI, protocol shape; is cheap per flow but must avoid collateral damage |
| A2 | **Active prober** | connects to any address it sees, replays captured packets, sends crafted first packets; tries to make a suspected node reveal itself |
| A3 | **Network crawler** | runs many nodes (Sybil), joins the DHT and PEX, collects every published record, address and key; may answer lookups with stale or forged data |
| A4 | **Malicious peer** | a node the victim talks to: relay, mailbox holder, exit, DHT neighbour, group or call member; deviates from the protocol at will |
| A5 | **Harvest-now-decrypt-later** | records traffic today and gains a cryptographically relevant quantum computer later; cannot forge signatures in real time |
| A6 | **Device compromise** | steals a device's keys at some point in time |

Out of scope: global passive adversaries correlating all traffic of the network (EchoFog is not a
mixnet); a censor that blocks all encrypted traffic or all UDP and all unknown TCP in a region
(EchoFog then needs domestic entry nodes, which are an operational, not a protocol, measure);
side channels on the user's device; bugs in the operating system and the cryptographic libraries.

## 3. Goals

1. **Unobservable carriers.** A passive observer cannot tell a noise3 flow from uniformly random UDP,
   even if it knows the node's identity and its cards (the selector needs the ECDH secret, and noise3
   keys are never published). A REALITY v2 or ws2 flow looks like a browser talking to the cover
   website, whose server answers it mirror (K1, V1). An active prober who knows a masking key can
   still confirm a node; this is why noise3 keys stay private and hidden nodes use per-invitation keys.
2. **No probing oracle.** A node never answers an unauthenticated packet as itself. Unauthenticated
   REALITY traffic goes to the cover site; unauthenticated noise3 traffic gets no answer (V1).
3. **Hidden nodes stay hidden.** A hidden node reveals nothing about itself, not even that it is an
   EchoFog node of a given identity, before the initiator proves it holds an invitation (K5).
4. **Everything a node says about itself is signed by it**, travels verbatim, and is never
   overwritten by third parties (K4).
5. **Explicit versions at every layer**, negotiated inside encrypted channels and bound to the
   handshake transcript, so that v3 can be added without a flag day (K3).
6. **Hybrid post-quantum key agreement** in the handshake and in chats (K10).
7. **One way to do each thing**: one frame format over every carrier, one padding mechanism per
   carrier, one node description, one time unit (seconds), one signed-object encoding (V6).
8. **Test vectors** for every deterministic construction, generated by an independent reference
   program.

## 4. Non-goals for v2

- Interoperability with v1 (0.2.x). A v2 node never speaks v1.
- Multipath scheduling (several paths used at once). v2 defines paths and migration; scheduling
  is a later feature ([registries.md](registries.md), feature `multipath`, reserved).
- MLS for groups, SPQR (post-quantum ratchet), header encryption, multi-device fan-out. v2 reserves
  their places (fields, feature IDs, labels) but does not specify them.
- A new payment contract. exit/2 carries the existing TON vault vouchers.
- TUN mode, routing rules and kill switch: client features, not protocol.
- Anonymity against A3 for *public* nodes: a public node must be dialable by anyone, so anyone can
  learn and confirm its addresses ([security.md](security.md) §4).

## 5. How the layers fit

```
 L3  services        dht/2  pex/2  chat/2  mailbox/2  relay/2  exit/2  files/2  call/2
                     ── one stream per service use, opened with OPEN{service, version} ──
 L2  session         features (u16), control stream, paths, PATH_BIND, PING, GOAWAY, REJECT
 L1  handshake       Noise XKhfs / XXhfs, X25519 + ML-KEM-768, ChaChaPoly, BLAKE2s
                     prologue = channel binding of the carrier + offered suites
     frame v2        type ‖ flags ‖ length ‖ payload, on every stream; datagrams: type ‖ payload
     carrier mux     QUIC streams and datagrams | byte-stream mux (MUX_* frames) for TCP/WS
     carrier TLS     TLS 1.3 (QUIC or TCP), X25519MLKEM768, exporter → channel binding
 L0  masking suite   noise3 (Elligator2 + AEAD over UDP) | reality2-quic | reality2-tcp | ws2
                     chosen from the peer's card; no version field on the wire
     IP              UDP / TCP

 L5  signed objects  card, prekeys, devices, mailbox-hint, name, release — one envelope format
 L6  storage         schema number, forward migrations (local only)
 L7  algorithms      every key and signature carries an algorithm tag (ed25519, x25519, mlkem768…)
 L4  messages        protobuf with evolution rules, canonical form for signed bodies
```

A connection is built bottom-up:

1. The initiator holds the responder's **node card** ([objects.md](objects.md) §3), obtained from an
   invitation link, the DHT, PEX or a bootstrap list. The card lists the responder's addresses, each
   with an L0 suite, a masking key and suite parameters (such as the REALITY cover SNI).
2. The initiator opens an **L0 carrier** to one address ([wire.md](wire.md) §2–§5). Inside the
   carrier there is TLS 1.3 (QUIC for UDP suites, TLS over TCP for TCP suites). TLS here provides
   transport encryption and the **channel binding**; it does not authenticate anyone (certificates
   are not checked).
3. On the first stream the initiator sends `HS_HELLO` and the first **Noise** message
   ([handshake.md](handshake.md)). XK is used because the card gives the responder's handshake
   static key; XX is used only for bootstrap when no card is known. The handshake authenticates both
   nodes, binds the carrier's TLS session, negotiates features (L2) and services (L3), and may carry
   0-RTT data.
4. The first stream becomes the **control stream** of the session. Further streams start with
   `OPEN{service, version}`; datagrams carry a type and a payload ([wire.md](wire.md) §6).
5. A session can gain more **paths**: another carrier connection bound with `PATH_BIND`, or a relay
   circuit carrying sealed records ([wire.md](wire.md) §8). Paths can change without a new handshake.

### 5.1 Keys a node holds

| Key | Algorithm | Purpose | Published in |
|---|---|---|---|
| account key | Ed25519 (alg `ed25519`), from the mnemonic | the person's identity: node ID = SHA-256 of its algorithm-tagged form; signs the device list | device list envelope, `account` in cards |
| device key | Ed25519, random per installation | signs the device's card and prekeys; device ID = SHA-256 of its algorithm-tagged form | card envelope |
| handshake static key `hs` | X25519, random per device | Noise static key `s` | card (`hs_key`) |
| masking keys | X25519, random; a contacts key, one per unredeemed invitation, one per REALITY listener | L0: noise3 and REALITY v2 | card (`mask_keys`); noise3 keys only in peer and invitation cards |
| prekeys | X25519 IK/SPK/OPK + ML-KEM-768, per device | chat/2 PQXDH | `prekeys/2` object |
| wallet key | TON | exit payments | never on the wire except in vouchers |

The account key is derived from the user's BIP-39 mnemonic exactly as v1's identity key (SLIP-0010
Ed25519, path `m/44'/999'/account'/0'`). The device key, the handshake static key and the masking keys
are **random**, are stored encrypted on disk, and MUST NOT be derived from the account key or the
node ID (K1). Restoring the mnemonic on a new installation keeps the node ID and creates a new device.

### 5.2 Node ID and device ID

`node_id = SHA-256(akey)` where `akey` is the algorithm-tagged public **account** key
([objects.md](objects.md) §1; [registries.md](registries.md) §8). For Ed25519, `akey = 0x01 ‖ pub`
(33 bytes). `device_id` is the same hash of a device key. The text form of either is 64 lowercase hex
characters. Contacts, links and chats address node IDs; the network (DHT positions, sessions, relays,
the handshake's responder binding) addresses device IDs. In v2.0 every account has one device.

**Decision D1 (node ID derivation).** v1 used `SHA-256(pub)`. v2 hashes the algorithm-tagged key so
that IDs of keys of different algorithms (for example a later ML-DSA identity) can never collide or be
confused. This gives everyone a new node ID; with no v1 users this costs nothing, and it is the only
moment when the change is free. Together with Decision D35 (separate account and device keys) the node
ID never has to change again when multi-device ships.

## 6. Conventions

- Byte strings are written in hex; `‖` is concatenation; `[a:b]` is the slice from byte `a`
  (inclusive) to `b` (exclusive).
- `u8`, `u16`, `u32`, `u64` are unsigned big-endian integers unless a field says `le`.
- `varint` is the QUIC variable-length integer (RFC 9000 §16): the two high bits of the first byte
  give the length (1, 2, 4 or 8 bytes). Encoders MUST use the shortest form; decoders MUST reject a
  longer form than necessary.
- Time is unix seconds (UTC) everywhere on the wire, as `u64` unless stated. Nanoseconds are never
  used.
- `HKDF-SHA256(ikm, salt, info, L)` is RFC 5869 with SHA-256. An empty salt means 32 zero bytes, as
  RFC 5869 specifies. `HMAC-SHA256(key, msg)` is RFC 2104.
- Inside the Noise handshake only, `HASH` is BLAKE2s-256 and `HKDF` is the Noise HKDF built on
  HMAC-BLAKE2s ([handshake.md](handshake.md) §2).
- `AEAD` is ChaCha20-Poly1305 (RFC 8439) unless stated; the REALITY v2 token uses AES-256-GCM.
- Every label is an ASCII string from [registries.md](registries.md) §7. Wherever a label is input
  to a hash, MAC or KDF, it is followed only by fixed-length fields or by length-prefixed fields, so
  no two inputs can be confused.
- Protobuf (proto3) is used for message bodies. Field numbers and types are given in each section;
  the canonical form required for signed bodies is defined in [objects.md](objects.md) §2.
- Examples use documentation addresses only: `192.0.2.0/24`, `198.51.100.0/24`, `2001:db8::/32`,
  `example.org`.

## 7. Decisions

Where the design draft left a choice open, this specification takes the safer option and records it
as a numbered **Decision**. The full list is in [CHANGES-from-v1.md](CHANGES-from-v1.md) §3; each
decision is stated in the section it affects. Choices that need a product decision are listed as
**Open questions** in [CHANGES-from-v1.md](CHANGES-from-v1.md) §4.

An independent audit of the first draft (November 2026) found 1 critical, 6 high, 22 medium and 17
low issues; how each was handled is recorded in [CHANGES-from-v1.md](CHANGES-from-v1.md) §5.
