# L3 services

Each service is versioned on its own ([versioning.md](versioning.md) §3.4). A service is used by
opening a stream with `OPEN{service, version}` ([wire.md](wire.md) §6.3); its frame types start at
`0x40` and are listed with their maximum payload sizes in [registries.md](registries.md) §4 (the
"Max" columns below are the same values).

## 1. Common rules

- Service frame payloads are canonical protobuf messages (L4 rules), one message per frame, unless a
  section gives a byte layout.
- Requests that expect an answer carry `uint64 req = 1`; the answer echoes it. Many requests may share
  one stream; answers may come out of order.
- Each service lists its timeouts. A request that times out is abandoned; the stream stays usable.
- A frame longer than its type's maximum ends the stream with `CLOSE{frame-too-large}`.
- Errors end a stream with `CLOSE{code}`; service-specific codes are `0x100 + n` and listed per
  service. Malformed frames of a known type MAY raise the peer's abuse score; unknown types, unknown
  fields and UNSUPPORTED never do.
- Every limit below is per session unless stated; nodes also apply the same limits per network
  diversity unit (IPv4 /24, IPv6 /32; [dht.md](dht.md) §1) with a factor of 3, and per authenticated
  node ID per day where stated, so new sessions do not reset quotas.
- Text forms: `dht/2`, `pex/2`, `chat/2`, `mailbox/2`, `relay/2`, `exit/2`, `files/2`, `call/2`.

## 2. dht/2 (service 1)

Carries the DHT of [dht.md](dht.md). One long-lived bidirectional stream per session is enough.

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | FIND_NODE | `{ uint64 req = 1; bytes target = 2; }` (32 bytes) | 64 |
| 0x41 | NODES | `{ uint64 req = 1; repeated bytes cards = 2; }` ≤ 20 signed cards | 65536 |
| 0x42 | FIND_VALUE | `{ uint64 req = 1; bytes key = 2; }` | 64 |
| 0x43 | VALUE | `{ uint64 req = 1; bytes record = 2; repeated bytes cards = 3; }` record or closer cards | 65536 |
| 0x44 | STORE | `{ uint64 req = 1; bytes record = 2; }` | 24576 |
| 0x45 | STORED | `{ uint64 req = 1; uint32 status = 2; }` 0 stored, 1 stale (lower seq), 2 refused, 3 quota, 4 invalid | 32 |
| 0x46 | ERROR | `{ uint64 req = 1; uint32 code = 2; }` | 32 |

- 0-RTT safe: FIND_NODE and FIND_VALUE ([handshake.md](handshake.md) §10).
- Timeout 2 s per request. Rate: 20 requests/s per session (burst 60); STORE quotas in
  [dht.md](dht.md) §3.
- Codes: 0x100 `bad-record`, 0x101 `wrong-key`.
- Later: Negentropy-style set reconciliation for replica repair.

## 3. pex/2 (service 2)

Peer exchange of **signed public cards only**.

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | GET | `{ uint64 req = 1; uint32 max = 2; uint32 want_flags = 3; uint32 want_role = 4; string region = 5; bool release = 6; }` | 64 |
| 0x41 | CARDS | `{ uint64 req = 1; repeated bytes cards = 2; bytes release = 3; }` ≤ 16 cards, optional release manifest | 98304 |

- `want_flags` uses the card flag bits ([registries.md](registries.md) §11): for example `exit` asks for
  exit cards (this replaces v1's `TypeExitDirectory`), `relay` for relays. `region` filters exits.
  `release = true` asks a server for the latest release manifest it holds.
- The answering node returns only public-scope cards it verified, that are not `unlisted`, and whose
  node it reached at an address of that card in a working session within 24 hours. It never includes
  cards of the asking node. Its own card comes first.
- Lazy PEX (V2): a node sends GET only when it has fewer than 8 working peers or no relay, after a
  `uniform(1, 10)` s delay from session start, at most once per 30 s per session. An answering node
  enforces 1 GET per 30 s per session (burst 2).
- A received card is only a *candidate*: it enters the peer store's "new" table, separate from the
  "tried" table of nodes reached at least once; dial selection prefers "tried" (as Bitcoin's addrman).
  A card never yet reached is not a guard candidate.
- 0-RTT safe: GET. Timeout 4 s.

## 4. chat/2 (service 3)

Personal end-to-end messages. The key agreement and ratchet are the v1 PQXDH and Double Ratchet,
unchanged (Decision D13); v2 changes how they are carried.

### 4.1 Frames

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | DELIVER | `{ uint64 req = 1; bytes envelope = 2; }` a `SealedEnvelope` ≤ 61440 bytes | 61504 |
| 0x41 | DELIVERED | `{ uint64 req = 1; uint32 status = 2; }` 0 ok, 1 duplicate (also ok), 2 no-session, 3 too-large, 4 refused | 32 |
| 0x42 | BUNDLE_GET | `{ uint64 req = 1; }` | 16 |
| 0x43 | BUNDLE | `{ uint64 req = 1; bytes prekeys = 2; }` a `prekeys/2` object with one fresh one-time prekey | 8256 |

Timeouts: DELIVER 15 s, BUNDLE_GET 5 s.

### 4.2 Sealed sender

```
message SealedEnvelope {
  bytes to = 1;           // recipient node ID (32)
  bytes eph = 2;          // X25519 ephemeral public key (32)
  bytes box = 3;          // ChaCha20-Poly1305(k, nonce 0^12, aad = to ‖ eph, Inner)
}
k = HKDF-SHA256(ikm = X25519(eph_sk, IK_recipient), salt = empty,
                info = "echofog/chat2/seal" ‖ eph(32) ‖ IK_recipient(32), L = 32)

message Inner {
  bytes  sender = 1;      // akey of the sender's identity key
  bytes  id = 2;          // 16-byte message ID
  uint64 time = 3;
  oneof body {
    bytes pqxdh_init = 4; // X3DHInit (v1 message, with KEM fields), first message of a session
    bytes ratchet = 5;    // SecureMessage (v1 message)
  }
}
```

`IK_recipient` is `ik` from the recipient's `prekeys/2`. Relays and mailboxes see only `to`, the
ephemeral key and the size (K7). The receiver checks that `sender` matches the identity bound by the
PQXDH init or by the ratchet session; a mismatch drops the message.

### 4.3 Plaintext

The ratchet plaintext is v1's `PersonalMessageProto` with two new fields:

- `Control control = 11` — knock, reset, read receipt and delivery receipt become typed fields instead
  of text prefixes (`fogknock1:`, `fogreset1:`, `fogack1:` are retired; K3);
- `bytes pad = 15` — the serialised plaintext is padded to the next of 256, 1024, 4096, 16384 or 57344
  bytes (K7). A message that would exceed 57344 bytes is sent as a file (files/2). With ratchet,
  init and envelope overheads, every envelope fits in 61440 bytes and so in one mailbox item.

### 4.4 Rules

- Replays and undecryptable duplicates are dropped silently; a decryption failure never deletes a
  session or triggers a reset (K8). If three distinct messages from one sender fail within 10 minutes,
  the receiver fetches the sender's prekeys and starts a fresh PQXDH session itself; the old session is
  kept 7 days for late messages.
- Every v2 bundle has a KEM prekey and every init MUST use it; a classic X3DH init is refused (no
  downgrade window; audit P1). The label `FOG_X3DH_V2` is therefore retired in v2.
- DELIVER: 20/s per session. BUNDLE_GET: 2 per minute per session, direct sessions only.
- Delivery when the recipient is offline: mailbox/2 (§5).
- Later: SPQR (`spqr`), encrypted ratchet headers (`sealed-header`), per-device fan-out, MLS groups
  (`mls`). Group logs keep travelling inside chat/2 plaintexts as in v1.

## 5. mailbox/2 (service 4)

Store-and-forward with two hops: the sender's **forwarding relay** F is not the recipient's
**mailbox relay** M. F learns the sender and M; M learns F and a queue ID; neither learns both ends
(K7, K9).

### 5.1 Frames

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | QUEUE_CREATE | `{ uint64 req = 1; bytes queue = 2; bytes auth_key = 3; uint32 ttl = 4; bool public = 5; uint32 pow_bits = 6; }` | 128 |
| 0x41 | QUEUE_OK | `{ uint64 req = 1; uint64 expires = 2; }` | 32 |
| 0x42 | DEPOSIT | `{ uint64 req = 1; bytes queue = 2; bytes blob = 3; uint64 pow = 4; }` blob ≤ 65536 | 65664 |
| 0x43 | DEPOSITED | `{ uint64 req = 1; uint32 status = 2; bytes item = 3; }` 0 stored, 1 unknown-queue, 2 full, 3 too-large, 4 refused, 5 relay-unreachable, 6 pow-required | 48 |
| 0x44 | FETCH | `{ uint64 req = 1; bytes queue = 2; bytes after = 3; uint32 max = 4; uint32 wait = 5; bytes auth = 6; }` | 160 |
| 0x45 | ITEMS | `{ uint64 req = 1; repeated Item items = 2; bool more = 3; }`, `Item { bytes id = 1; bytes blob = 2; uint64 stored = 3; }` | 1048576 |
| 0x46 | ACK | `{ uint64 req = 1; bytes queue = 2; repeated bytes ids = 3; bytes auth = 4; }` ≤ 100 IDs | 2048 |
| 0x47 | FORWARD | `{ uint64 req = 1; bytes relay = 2; bytes relay_card = 3; bytes sealed = 4; }` | 73728 |
| 0x48 | ERROR | `{ uint64 req = 1; uint32 code = 2; }` | 32 |

Timeouts: QUEUE_CREATE 10 s, DEPOSIT 10 s, FORWARD 20 s, FETCH `wait` + 5 s, ACK 10 s.

### 5.2 Queues

- The recipient creates queues on its mailbox relays: random 16-byte `queue`, an Ed25519 `auth_key`
  per queue, `ttl` ≤ 30 days. Contacts learn `(M, queue)` inside end-to-end messages; a `public` queue
  is advertised in the `mailbox-hint/2` object for first contact.
- Queues rotate: a new private queue every `uniform(20, 28)` hours, announced to contacts; the old one
  is kept until drained plus 7 days.
- `auth` of FETCH and ACK: Ed25519 signature by the queue's `auth_key` over
  `"echofog/mailbox2/fetch-auth" ‖ sid(16) ‖ u8(op: 1 FETCH, 2 ACK) ‖ queue(16) ‖ u64(req)`. It is
  bound to the session, so it cannot be replayed elsewhere.
- **Public queues** accept a deposit only with proof of work:
  `SHA-256("echofog/mailbox2/pow" ‖ queue(16) ‖ SHA-256(blob) ‖ u64(pow))` with at least `pow_bits`
  leading zero bits (default 20, at most 26; the hint shows it). The sender computes it; M checks it.
  A public queue holds at most 100 items and 2 MiB, separate from private queues, so a flood of first
  contacts never blocks contacts' mail.

### 5.3 Delivery

1. The sender picks F (any relay it trusts, normally one of its own guards or servers) and sends
   `FORWARD{relay = M, sealed}`.
   ```
   sealed = eph(32) ‖ ChaCha20-Poly1305(k, nonce 0^12, aad = eph, DEPOSIT body)
   k      = HKDF-SHA256(ikm = X25519(eph_sk, hs_M), salt = empty,
                        info = "echofog/mailbox2/forward" ‖ eph(32) ‖ hs_M(32), L = 32)
   ```
   `hs_M` is M's handshake static key from M's card. F cannot read the queue ID.
2. F reaches M over an existing session, or dials M only with a card it verified itself (from the DHT
   or its own contacts). A `relay_card` supplied by the sender is used only if it is signed by `relay`,
   and F never dials private, loopback, link-local or reserved addresses from it; F makes at most 10
   new dials per minute for one sender session. F opens mailbox/2 on its session with M and sends the
   same `FORWARD`. M recognises `relay = self`, opens `sealed`, and stores the deposit. The result flows
   back as `DEPOSITED` through F.
3. M keeps `SHA-256(sealed)` for 7 days; a repeated `sealed` is answered `stored` without storing a
   second copy (replays by F or an on-path party add nothing).
4. The recipient fetches with `FETCH` (long-poll: `wait` ≤ 60 s) and deletes with `ACK` only after it
   has processed the items. Unacknowledged items stay until their 7-day expiry (K9: v1 deleted on push
   without a receipt).
5. One hop only: M never forwards a deposit further. There is no random walk (K9).

### 5.4 Limits

- Per private queue 500 items and 10 MiB; per item 64 KiB; M holds queues only for nodes seen within
  30 days; 100 MiB and 100 000 items in total by default.
- Quotas count the **node ID** that hands the deposit to M (F) and its diversity unit, **per day**:
  5000 items and 50 MiB per F node ID, 3 times that per diversity unit. At F, the same per day for each
  sender node ID. A new session or a new handshake does not reset a quota. A large F shares its quota
  among its users; F applies its own per-sender quotas so that one sender cannot exhaust it.
- Codes: 0x100 `pow-invalid`.
- Later: paid or token-gated deposits (rendezvous tokens, relay payment) in addition to proof of work,
  push notifications.

## 6. relay/2 (service 5)

Circuits through a relay R between two nodes A and B that are both connected to R. Inside the circuit
A and B run their own end-to-end handshake (XK, A holds B's card), so R carries only sealed records
([wire.md](wire.md) §8.4).

### 6.1 Frames

| Type | Frame | Direction | Body | Max |
|---|---|---|---|---|
| 0x40 | CONNECT | A → R | `{ uint64 req = 1; bytes target = 2; uint32 mode = 3; bytes token = 4; uint32 target_kind = 5; bool want_addr = 6; }` | 256 |
| 0x41 | INCOMING | R → B | `{ uint64 cid = 1; bytes source = 2; uint32 mode = 3; bytes target = 4; bool want_addr = 5; }` | 128 |
| 0x42 | ACCEPT | B → R | `{ uint64 cid = 1; bool want_addr = 2; }` | 32 |
| 0x43 | REFUSE | B → R | `{ uint64 cid = 1; }` | 16 |
| 0x44 | CONNECTED | R → A | `{ uint64 req = 1; uint64 cid = 2; bytes target = 3; }` | 64 |
| 0x45 | REFUSED | R → A | `{ uint64 req = 1; bytes target = 2; uint32 reason = 3; }` | 64 |
| 0x46 | REGISTER | B → R | `{ uint64 req = 1; repeated bytes ids = 2; uint32 day = 3; }` ≤ 256 IDs | 8320 |
| 0x47 | REGISTERED | R → B | `{ uint64 req = 1; uint32 count = 2; }` | 32 |
| 0x48 | PEER_ADDR | R → A, B | `{ uint64 cid = 1; bytes addr = 2; }` family(1) ‖ ip ‖ port(2) | 32 |

Timeouts: CONNECT 8 s, REGISTER 5 s.

### 6.2 Circuits

- `mode`: 1 stream circuit, 2 datagram circuit, 3 signal circuit (a stream circuit limited to 64 KiB
  and 2 minutes, for coordinating hole punching).
- **One circuit ID.** R chooses a random 64-bit `cid`, unique at R for 24 hours, and sends the same
  value to both ends (INCOMING and CONNECTED). Both use it for the channel binding
  ([handshake.md](handshake.md) §3.2) and in `RELAY_DGRAM`.
- `source` is the node ID R authenticated on its session with A. B does not rely on it for security:
  the end-to-end handshake inside authenticates A (K8: v1 accepted an unsigned "source" and let any
  neighbour hijack routes).
- After CONNECTED and ACCEPT, the two relay streams are spliced: R copies bytes, nothing else. The
  bytes are records ([wire.md](wire.md) §8.4): kind-0 records carry `HS_HELLO` and the handshake, then
  everything is sealed.
- Datagram circuits: `RELAY_DGRAM{cid, payload}` datagrams; R forwards the payload (a `SEALED`
  datagram) unchanged to the other end.
- `token` is reserved for paid relaying; v2 relays ignore it.

### 6.3 Targets and the presence oracle

- `target_kind = 0`: `target` is a node ID. R serves it only for nodes connected to R whose card is
  not `unlisted`.
- `target_kind = 1`: `target` is a **rendezvous ID**. Hidden and unlisted nodes register at their relays
  only rendezvous IDs, never their node ID:
  `rid = HMAC-SHA256(S_c, "echofog/relay2/rendezvous" ‖ u32(day))`, one per contact, where `S_c` is the
  contact's personal pass secret ([handshake.md](handshake.md) §5) and `day = floor(unix / 86400)`.
  A contact computes the same value; nobody else can. Registrations are renewed daily (for the current
  and the next day).
- **Uniform refusals.** Whether the target is unknown, unreachable, refuses (REFUSE), or is blocked by
  policy, R answers `REFUSED{reason = 1 unavailable}` after `uniform(1, 4)` s from the CONNECT. The only
  other reason is 2 `limit`, about the requester's own limits. A prober learns nothing about a target
  it cannot already name with a rendezvous ID.

### 6.4 Hole punching

Hole punching turns a relayed session into one with a direct noise3 path. It is allowed only where
noise3 is allowed (same country, Decision D21) and needs feature `path-migration`.

1. A asks for addresses with `CONNECT{want_addr = true}`; B agrees with `ACCEPT{want_addr = true}`. Only
   then does R send `PEER_ADDR` with each end's observed public address to the other.
2. Inside the end-to-end session, A (the session initiator) sends `PUNCH_OFFER{addrs, nonce}`; B
   answers with its own `PUNCH_OFFER` with the same nonce. Each side accepts at most 4 targets from the
   other, and only addresses whose IP equals the IP in `PEER_ADDR` or appears in the peer's card;
   private (LAN) addresses only if both `PEER_ADDR` IPs are equal. Nothing else is ever a target (V4).
3. A measures the session RTT with PING, sends `PUNCH_SYNC{nonce}`, and starts after RTT/2; B starts on
   receipt of PUNCH_SYNC.
4. A, the noise3 initiator, sends fresh Init datagrams (wire.md §2.4) to each target using the noise3
   key of B's peer card. B sends 3–6 junk datagrams of `uniform(32, 256)` bytes, `uniform(10, 40)` ms
   apart, to each of A's targets to open its NAT mapping, and accepts A's Inits on its noise3 listener.
   A sends at most 24 datagrams per attempt.
5. The first noise3 flow that completes its inner QUIC handshake becomes a carrier; A sends `PATH_BIND`
   on its stream 0 ([wire.md](wire.md) §8.2). No new Noise handshake is needed.
6. Each attempt has 5 s; at most 2 attempts, then the session stays relayed and may try again after
   `uniform(90, 150)` s. PUNCH_OFFER at most once per 30 s per session.
7. Symmetric NATs (birthday-style punching with many ports) are the reserved feature `birthday-punch`.

### 6.5 Limits

Relay side: 16 circuits per session, 32 per source IP, 200 in total; idle 5 minutes; 4 GiB per
circuit; 16 GiB per peer per day; 50 Mbit/s per peer; 256 registered rendezvous IDs per node.

Later: relay as a QUIC-level path ("relay is a path"), multi-hop circuits, paid relaying.

## 7. exit/2 (service 6)

The VPN exit. Streams are opened by the client to an exit whose card has the `exit` flag.

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | TCP_CONNECT | `{ string host = 1; uint32 port = 2; uint32 flags = 3; }` | 288 |
| 0x41 | RESULT | `{ uint32 code = 1; }` codes in [registries.md](registries.md) §10.4 | 8 |
| 0x42 | DATA | raw bytes (payload is the data, not protobuf) | 16384 |
| 0x43 | UDP_OPEN | `{ uint64 req = 1; uint32 flags = 2; }` | 16 |
| 0x44 | UDP_OK | `{ uint64 req = 1; uint32 session = 2; uint32 code = 3; }` | 32 |
| 0x45 | UDP_PKT | `{ string addr = 1; bytes data = 2; }` (stream fallback for UDP) | 65800 |
| 0x46 | ACCOUNT_GET | `{ }` | 0 |
| 0x47 | ACCOUNT | `Account` (below) | 2048 |
| 0x48 | VOUCHER | `{ uint64 req = 1; bytes owner = 2; bytes pubkey = 3; bytes cumulative = 4; bytes sig = 5; bytes code_hash = 6; }` | 512 |
| 0x49 | VOUCHER_RESULT | `{ uint64 req = 1; bool ok = 2; uint32 error = 3; uint64 credit = 4; bytes cumulative = 5; }` | 64 |

Timeouts: TCP_CONNECT 10 s to RESULT, UDP_OPEN 5 s, ACCOUNT_GET 5 s, VOUCHER 20 s.

- **Client identity.** A client MUST connect to an exit under a **per-exit client identity**: a
  separate Ed25519 key per exit (random, stored locally), with a minimal unlisted peer card. The exit
  never learns the client's messenger identity; access comes from payment, not identity.
- TCP: the client sends TCP_CONNECT and MAY send DATA frames immediately (fast open). The exit
  answers RESULT, then DATA in both directions; stream FIN is TCP FIN. `host` ≤ 253 bytes.
- UDP over datagrams: `EXIT_UDP` payload is
  `session(varint) ‖ pkt(u16) ‖ frag_idx(u8) ‖ frag_count(u8) ‖ [addr_len(u8) ‖ addr] ‖ data`, where the
  address part is present only in fragment 0 (V3). At most 8 fragments; the exit reassembles at most 64
  incomplete packets per session and drops them after 2 s.
- Accounting: the exit pushes ACCOUNT when the credit crosses 50 %, 10 % and 0 of the last purchase, and
  every `uniform(45, 75)` s while tunnels are open; clients do not poll (V2).
  ```
  message Account {
    uint64 price = 1;            // must equal the card's ExitOffer.price; clients refuse to auto-pay otherwise (V5)
    uint64 credit = 2;           // bytes
    uint64 free_left = 3;        // bytes
    string pay_address = 4;      // only to served clients (K6)
    string pay_tag = 5;          // "ef2:" + 16 hex, random per (exit, client identity), stored by the exit
    uint64 min_pay = 6;
    repeated uint32 assets = 7;
    bytes vault = 8;             // vault parameters, as in v1, protobuf-encoded
  }
  ```
- Vouchers use the existing TON vault contract and voucher cell (`EFV1`). `code_hash` names the vault
  code; exits accept only codes on their allow-list. An exit caps unpaid credit per vault at
  `min(deposit, 0.05 TON)` (V5). The vault owner and key are the same for every exit the client pays,
  so colluding exits (and the chain) can link a client across exits; see Open question OQ5.
- The exit resolves names with DoH or DoT and a cache, never with the host's plaintext resolver (K6).
  It checks the **resolved** address (after DNS, against rebinding) and refuses private, loopback,
  link-local and reserved destinations.
- Rate limits per client identity: 20 new TCP connections per second, 256 distinct TCP targets per
  minute; 64 UDP sessions, 256 targets per UDP session, 2000 UDP packets per second (V4).
- `unreachable` covers refused, reset and timed out alike (Decision D19). This hides "closed" from
  "filtered"; it does not hide "open" (`ok`), and response times still differ. An exit MAY delay
  `unreachable` to a uniform time within its connect timeout.
- Padding: exit streams use PADDING frames according to the exit address's shape profile, including on
  hidden exits (V2: v1 hidden exits did not pad at all).
- Codes: 0x100 `payment-required`, 0x101 `policy`.
- Later: vault contract v2 with code version and per-exit payment keys (K6, V5), MASQUE, resumable
  tunnels across paths.

## 8. files/2 (service 7)

File transfer between two nodes over their session (direct or relayed). The offer travels inside a
chat/2 message: file ID (16 bytes), size, Merkle root, chunk size 65536, name, MIME type, sender node
ID.

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | WANT | `{ bytes fid = 1; repeated Range ranges = 2; }`, `Range { uint64 start = 1; uint32 count = 2; }`, ≤ 256 chunks per WANT | 4096 |
| 0x41 | CHUNK | `{ bytes fid = 1; uint64 index = 2; repeated bytes proof = 3; bytes data = 4; }` | 67584 |
| 0x42 | CANCEL | `{ bytes fid = 1; uint32 reason = 2; }` 1 cancelled, 2 declined, 3 unavailable | 32 |
| 0x43 | DONE | `{ bytes fid = 1; }` | 32 |

- Integrity: the Merkle tree of v1 (leaf = SHA-256(0x00 ‖ chunk), node = SHA-256(0x01 ‖ l ‖ r), RFC
  6962 shape); each CHUNK carries its audit path to the offered root.
- Confidentiality and replay protection come from the session; there is no per-transfer key. The
  sender serves only sessions whose authenticated peer is a recipient named by the offer (in a group:
  each member, over its own session). P7: v1 used one key for all group members and a frame handler
  that did not know the sender.
- Flow control as in v1 (AIMD window 16–256 chunks, RTO `max(1.5 s, 3·srtt)`), resume from a bitmap.
- Later: multi-source fetching, encrypted store-and-forward of files through mailboxes.

## 9. call/2 (service 8)

Voice, video and screen sharing, relayed through a tree of participants. Signalling and media keys
travel only over end-to-end sessions between participants (direct or sealed relay paths), so relays
outside the call see neither (P8).

### 9.1 Signalling

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | SIGNAL | `{ bytes origin = 1; bytes body = 2; bytes sig = 3; }` | 65536 |

- `body` is a `CallSignal` protobuf with the v1 actions as an enum (tree manifest, heartbeats, join,
  leave, stream announcements, subscriptions, NACK, feedback, …) and the v1 fields as typed fields.
- Signals forwarded beyond the first hop (manifests, announcements, joins) are signed by the
  originator's identity: `sig = Ed25519(origin key, "echofog/call2/signal" ‖ body)`, over the exact bytes
  (P8: v1 signed a re-serialised JSON). Hop-local signals (ping, NACK) are authenticated by the session
  and carry no signature.
- A **join** signal carries the joiner's per-call media signing key (a fresh Ed25519 key per join) and
  a fresh 16-byte `join_salt`.
- `CALL_SIGNAL` datagrams duplicate lossy signals (NACK, reports) for low latency. Timeout for
  request-like signals: 2 s.

### 9.2 Media packet v3

`CALL_AUDIO` / `CALL_VIDEO` datagram payload:

```
kind(u8) ‖ epoch(u8) ‖ sender(u8) ‖ stream(u8) ‖ gen(u8) ‖ seq(u32) ‖ ts(u32) ‖ frag_idx(u8) ‖ frag_count(u8) ‖ fragment
```

- `kind`: 1 audio, 2 video, 3 heartbeat, 4 screen audio, 6 video FEC. `sender` is the sender's index
  in the current tree manifest (replaces v1's 32-byte ID), `stream` its stream index, `gen` the low 8
  bits of the sender's join counter in this call.
- **Confidentiality** against everyone outside the call: AES-256-GCM under a per-sender, per-join key
  ```
  k_sender = HKDF-SHA256(ikm = epoch_secret, salt = empty,
                         info = "echofog/call2/sender-key" ‖ sender_node_id(32) ‖ join_salt(16), L = 32)
  nonce    = stream(u8) ‖ kind(u8) ‖ 0x0000 ‖ seq(u32) ‖ 0x00000000
  aad      = the unit header (the fields before frag_idx)
  ```
  Because every join has a new `join_salt`, a sender that restarts or rejoins and starts `seq` at 0
  again uses a new key: no nonce is reused (audit: rejoin in the same epoch). A sender MUST NOT send
  media until it has received the current epoch secret after its (re)join.
- **Source authentication** (P1): every member knows `epoch_secret` and could compute anyone's
  `k_sender`, so the key does not authenticate the sender. Each unit therefore carries a signature by
  the sender's per-call media key:
  ```
  unit = ciphertext ‖ tag ‖ sig(64)              (the nonce comes from the header and is not sent)
  sig  = Ed25519(media_key, "echofog/call2/unit" ‖ header ‖ ciphertext ‖ tag)
  ```
  The unit is split into at most 16 fragments. A receiver or relay accepts a unit only after the
  signature verifies with the media key announced in the sender's signed join.
- Replay windows mark a fragment only after its unit has been verified (P1: v1 marked before
  authentication, which let anyone "mute" a sender by pre-empting sequence numbers; with signatures, an
  insider can no longer forge units either). Relays MAY forward fragments before the unit is complete,
  but MUST NOT mark them as seen until it verifies.
- `epoch_secret` of epoch 0 is `HKDF-SHA256(ikm = room secret, info = "echofog/call2/epoch" ‖ u32(0))`.
  On every join, leave or kick the coordinator draws a new random epoch secret, sends it to each
  participant over its end-to-end session, and switches `uniform(1.5, 2.5)` s later; a kicked member
  never receives it.
- NACK retransmissions have a budget per child (at most 2× the child's recent receive rate); claimed
  relay capacity is capped by measured upload (P1).
- Later: MLS epochs (`mls`), SFrame-style headers, signature batching over several units, a
  constant-rate shaping profile for calls (P9), a WebRTC-shaped carrier for cross-border calls.
