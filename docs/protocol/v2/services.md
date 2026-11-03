# L3 services

Each service is versioned on its own ([versioning.md](versioning.md) §3.4). A service is used by
opening a stream with `OPEN{service, version}` ([wire.md](wire.md) §6.3); its frame types start at
`0x40` and are listed in [registries.md](registries.md) §4.

## 1. Common rules

- Service frame payloads are canonical protobuf messages (L4 rules), one message per frame, unless a
  section gives a byte layout.
- Requests that expect an answer carry `uint64 req = 1`; the answer echoes it. Many requests may share
  one stream; answers may come out of order.
- Each service lists its timeouts. A request that times out is abandoned; the stream stays usable.
- Errors end a stream with `CLOSE{code}`; service-specific codes are `0x100 + n` and listed per
  service. Malformed frames of a known type MAY raise the peer's abuse score; unknown types, unknown
  fields and UNSUPPORTED never do.
- Every limit below is per session unless stated; nodes also apply the same limits per source /24
  (IPv6 /48) with a factor of 3, so a Sybil cannot multiply them for free.
- Text forms: `dht/2`, `pex/2`, `chat/2`, `mailbox/2`, `relay/2`, `exit/2`, `files/2`, `call/2`.

## 2. dht/2 (service 1)

Carries the DHT of [dht.md](dht.md). One long-lived bidirectional stream per session is enough.

| Type | Frame | Body |
|---|---|---|
| 0x40 | FIND_NODE | `{ uint64 req = 1; bytes target = 2; }` (32 bytes) |
| 0x41 | NODES | `{ uint64 req = 1; repeated bytes cards = 2; }` ≤ 20 signed cards, ≤ 65536 bytes in total |
| 0x42 | FIND_VALUE | `{ uint64 req = 1; bytes key = 2; }` |
| 0x43 | VALUE | `{ uint64 req = 1; bytes record = 2; repeated bytes cards = 3; }` record or closer cards |
| 0x44 | STORE | `{ uint64 req = 1; bytes record = 2; string name = 3; }` `name` only for name-keyed kinds |
| 0x45 | STORED | `{ uint64 req = 1; uint32 status = 2; }` 0 stored, 1 stale (lower seq), 2 refused, 3 quota, 4 invalid |
| 0x46 | ERROR | `{ uint64 req = 1; uint32 code = 2; }` |

- 0-RTT safe: FIND_NODE and FIND_VALUE ([handshake.md](handshake.md) §10).
- Timeout 2 s per request. Rate: 20 requests/s per session (burst 60); STORE quotas in
  [dht.md](dht.md) §3.
- Codes: 0x100 `bad-record`, 0x101 `wrong-key`.
- Later: Negentropy-style set reconciliation for replica repair.

## 3. pex/2 (service 2)

Peer exchange of **signed cards only**.

| Type | Frame | Body |
|---|---|---|
| 0x40 | GET | `{ uint64 req = 1; uint32 max = 2; uint32 want_flags = 3; uint32 want_role = 4; string region = 5; }` |
| 0x41 | CARDS | `{ uint64 req = 1; repeated bytes cards = 2; }` ≤ 16 cards |

- `want_flags` uses the card flag bits ([registries.md](registries.md) §11): for example `exit` asks for
  exit cards (this replaces v1's `TypeExitDirectory`), `relay` for relays. `region` filters exits.
- The answering node returns only cards it verified, that are not `unlisted`, and whose node it reached
  in a working session within 24 hours. It never includes cards of the asking node. Its own card comes
  first.
- Lazy PEX (V2): a node sends GET only when it has fewer than 8 working peers or no relay, after a
  `uniform(1, 10)` s delay from session start, at most once per 30 s per session. An answering node
  enforces 1 GET per 30 s per session (burst 2).
- A received card is only a *candidate*: it enters the peer store's "new" table, separate from the
  "tried" table of nodes reached at least once; dial selection prefers "tried" (as Bitcoin's addrman).
  A card never yet reached is not a guard candidate.
- 0-RTT safe: GET.
- Timeout 4 s.

## 4. chat/2 (service 3)

Personal end-to-end messages. The key agreement and ratchet are the v1 PQXDH and Double Ratchet,
unchanged (Decision D13); v2 changes how they are carried.

### 4.1 Frames

| Type | Frame | Body |
|---|---|---|
| 0x40 | DELIVER | `{ uint64 req = 1; bytes envelope = 2; }` a `SealedEnvelope` |
| 0x41 | DELIVERED | `{ uint64 req = 1; uint32 status = 2; }` 0 ok, 1 duplicate (also ok), 2 no-session, 3 too-large, 4 refused |
| 0x42 | BUNDLE_GET | `{ uint64 req = 1; }` |
| 0x43 | BUNDLE | `{ uint64 req = 1; bytes prekeys = 2; }` a `prekeys/2` object with one fresh one-time prekey |

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
- `bytes pad = 15` — the serialised plaintext is padded to the next of 256, 1024, 4096, 16384 or 65536
  bytes (K7).

### 4.4 Rules

- Replays and undecryptable duplicates are dropped silently; a decryption failure never deletes a
  session or triggers a reset (K8). If three distinct messages from one sender fail within 10 minutes,
  the receiver fetches the sender's prekeys and starts a fresh PQXDH session itself; the old session is
  kept 7 days for late messages.
- Every v2 bundle has a KEM prekey; a classic X3DH init is refused (no downgrade window; audit P1).
- Envelope ≤ 72 KiB. DELIVER: 20/s per session. BUNDLE_GET: 2 per minute per session, direct sessions
  only.
- Delivery when the recipient is offline: mailbox/2 (§5).
- Later: SPQR (`spqr`), encrypted ratchet headers (`sealed-header`), per-device fan-out, MLS groups
  (`mls`). Group logs keep travelling inside chat/2 plaintexts as in v1.

## 5. mailbox/2 (service 4)

Store-and-forward with two hops: the sender's **forwarding relay** F is not the recipient's
**mailbox relay** M. F learns the sender and M; M learns F and a queue ID; neither learns both ends
(K7, K9).

### 5.1 Frames

| Type | Frame | Body |
|---|---|---|
| 0x40 | QUEUE_CREATE | `{ uint64 req = 1; bytes queue = 2; bytes auth_key = 3; uint32 ttl = 4; bool public = 5; }` |
| 0x41 | QUEUE_OK | `{ uint64 req = 1; uint64 expires = 2; }` |
| 0x42 | DEPOSIT | `{ uint64 req = 1; bytes queue = 2; bytes blob = 3; }` |
| 0x43 | DEPOSITED | `{ uint64 req = 1; uint32 status = 2; bytes item = 3; }` 0 stored, 1 unknown-queue, 2 full, 3 too-large, 4 refused, 5 relay-unreachable |
| 0x44 | FETCH | `{ uint64 req = 1; bytes queue = 2; bytes after = 3; uint32 max = 4; uint32 wait = 5; bytes auth = 6; }` |
| 0x45 | ITEMS | `{ uint64 req = 1; repeated Item items = 2; bool more = 3; }`, `Item { bytes id = 1; bytes blob = 2; uint64 stored = 3; }` |
| 0x46 | ACK | `{ uint64 req = 1; bytes queue = 2; repeated bytes ids = 3; bytes auth = 4; }` |
| 0x47 | FORWARD | `{ uint64 req = 1; bytes relay = 2; bytes relay_card = 3; bytes sealed = 4; }` |
| 0x48 | ERROR | `{ uint64 req = 1; uint32 code = 2; }` |

### 5.2 Queues

- The recipient creates queues on its mailbox relays: random 16-byte `queue`, an Ed25519 `auth_key`
  per queue, `ttl` ≤ 30 days. Contacts learn `(M, queue)` inside end-to-end messages; a `public` queue
  is advertised in the `mailbox-hint/2` object for first contact.
- Queues rotate: a new private queue at least every 24 hours, announced to contacts; the old one is kept
  until drained plus 7 days.
- `auth` of FETCH and ACK: Ed25519 signature by the queue's `auth_key` over
  `"echofog/mailbox2/fetch-auth" ‖ sid(16) ‖ u8(op: 1 FETCH, 2 ACK) ‖ queue(16) ‖ u64(req)`. It is
  bound to the session, so it cannot be replayed elsewhere.

### 5.3 Delivery

1. The sender picks F (any relay it trusts, normally one of its own guards or servers) and sends
   `FORWARD{relay = M, sealed}`.
   ```
   sealed = eph(32) ‖ ChaCha20-Poly1305(k, nonce 0^12, aad = eph, DEPOSIT body)
   k      = HKDF-SHA256(ikm = X25519(eph_sk, hs_M), salt = empty,
                        info = "echofog/mailbox2/forward" ‖ eph(32) ‖ hs_M(32), L = 32)
   ```
   `hs_M` is M's handshake static key from M's card. F cannot read the queue ID.
2. F opens mailbox/2 on its session with M and sends the same `FORWARD`. M recognises `relay = self`,
   opens `sealed`, and stores the deposit. The result flows back as `DEPOSITED` through F.
3. The recipient fetches with `FETCH` (long-poll: `wait` ≤ 60 s) and deletes with `ACK` only after it
   has processed the items. Unacknowledged items stay until their 7-day expiry (K9: v1 deleted on push
   without a receipt).
4. One hop only: M never forwards a deposit further. There is no random walk (K9).

### 5.4 Limits

Per queue 500 items and 10 MiB; per item 64 KiB; per depositing session 2000 items and 20 MiB per day;
M holds queues only for nodes seen within 30 days; 100 MiB and 100 000 items in total by default.
Quotas count the session that hands the deposit to M (F) and, at F, the sender's session — never an
unverified "origin" field.

Later: paid or token-gated deposits for sealed senders (rendezvous tokens, relay payment), push
notifications.

## 6. relay/2 (service 5)

Circuits through a relay R between two nodes A and B that are both connected to R. Inside the circuit
A and B run their own end-to-end handshake (XK, A holds B's card), so R carries only sealed records
([wire.md](wire.md) §8.4).

| Type | Frame | Direction | Body |
|---|---|---|---|
| 0x40 | CONNECT | A → R | `{ uint64 req = 1; bytes target = 2; uint32 mode = 3; bytes token = 4; }` |
| 0x41 | INCOMING | R → B | `{ uint64 cid = 1; bytes source = 2; uint32 mode = 3; }` |
| 0x42 | ACCEPT | B → R | `{ uint64 cid = 1; }` |
| 0x43 | REFUSE | B → R | `{ uint64 cid = 1; uint32 reason = 2; }` |
| 0x44 | CONNECTED | R → A | `{ uint64 req = 1; uint64 cid = 2; bytes target = 3; }` |
| 0x45 | REFUSED | R → A | `{ uint64 req = 1; bytes target = 2; uint32 reason = 3; }` |

- `mode`: 1 stream circuit, 2 datagram circuit, 3 signal circuit (a stream circuit limited to 64 KiB
  and 2 minutes, for coordinating hole punching inside the end-to-end session).
- `source` is the node ID R authenticated on its session with A. B does not rely on it for security:
  the end-to-end handshake inside authenticates A (K8: v1 accepted an unsigned "source" and let any
  neighbour hijack routes).
- After CONNECTED and ACCEPT, the two relay streams are spliced: R copies bytes, nothing else. The
  first bytes from A are `u16 len ‖ …` records carrying `HS_HELLO` and message 1 of the end-to-end
  handshake (the records are plaintext until the handshake completes, then sealed).
- Datagram circuits: `RELAY_DGRAM{cid, payload}` datagrams; R maps A's `cid` to B's and forwards the
  payload (a `SEALED` datagram) unchanged.
- `token` is reserved for paid relaying; v2 relays ignore it.
- A refusal names the target, so it cannot be confused with another pending CONNECT (K8).
- Hole punching: inside the relayed session, peers exchange candidate addresses only with each other,
  authenticated; LAN addresses only when both observed the same public IP (K7). A successful punch
  creates a direct carrier, bound with PATH_BIND.
- Limits (relay side): 16 circuits per session, 32 per source IP, 200 in total; idle 5 minutes; 4 GiB
  per circuit; 16 GiB per peer per day; 50 Mbit/s per peer. Reasons: 1 target-unknown,
  2 target-unreachable, 3 limit, 4 policy, 5 refused-by-target.
- Later: relay as a QUIC-level path ("relay is a path"), multi-hop circuits, paid relaying.

## 7. exit/2 (service 6)

The VPN exit. Streams are opened by the client to an exit whose card has the `exit` flag.

| Type | Frame | Body |
|---|---|---|
| 0x40 | TCP_CONNECT | `{ string host = 1; uint32 port = 2; uint32 flags = 3; }` |
| 0x41 | RESULT | `{ uint32 code = 1; }` codes in [registries.md](registries.md) §10.3 |
| 0x42 | DATA | raw bytes (payload is the data, not protobuf) |
| 0x43 | UDP_OPEN | `{ uint64 req = 1; uint32 flags = 2; }` |
| 0x44 | UDP_OK | `{ uint64 req = 1; uint32 session = 2; uint32 code = 3; }` |
| 0x45 | UDP_PKT | `{ string addr = 1; bytes data = 2; }` (stream fallback for UDP) |
| 0x46 | ACCOUNT_GET | `{ }` |
| 0x47 | ACCOUNT | `Account` (below) |
| 0x48 | VOUCHER | `{ uint64 req = 1; bytes owner = 2; bytes pubkey = 3; bytes cumulative = 4; bytes sig = 5; bytes code_hash = 6; }` |
| 0x49 | VOUCHER_RESULT | `{ uint64 req = 1; bool ok = 2; uint32 error = 3; uint64 credit = 4; bytes cumulative = 5; }` |

- TCP: the client sends TCP_CONNECT and MAY send DATA frames immediately (fast open). The exit
  answers RESULT, then DATA in both directions; stream FIN is TCP FIN. `host` ≤ 253 bytes.
- UDP over datagrams: `EXIT_UDP` payload is
  `session(varint) ‖ pkt(u16) ‖ frag_idx(u8) ‖ frag_count(u8) ‖ [addr_len(u8) ‖ addr] ‖ data`, where the
  address part is present only in fragment 0 (V3). At most 8 fragments.
- Accounting: the exit pushes ACCOUNT when the credit crosses 50 %, 10 % and 0 of the last purchase, and
  every `uniform(45, 75)` s while tunnels are open; clients do not poll (V2).
  ```
  message Account {
    uint64 price = 1;            // must equal the card's ExitOffer.price; clients refuse to auto-pay otherwise (V5)
    uint64 credit = 2;           // bytes
    uint64 free_left = 3;        // bytes
    string pay_address = 4;      // only to served clients (K6)
    string pay_tag = 5;          // "ef2:" + 16 hex, random per (exit, client), stored by the exit
    uint64 min_pay = 6;
    repeated uint32 assets = 7;
    bytes vault = 8;             // vault parameters, as in v1, protobuf-encoded
  }
  ```
- Vouchers use the existing TON vault contract and voucher cell (`EFV1`). `code_hash` names the vault
  code; exits accept only codes on their allow-list. An exit caps unpaid credit per vault at
  `min(deposit, 0.05 TON)` (V5).
- The exit resolves names with DoH or DoT and a cache, never with the host's plaintext resolver (K6).
  It refuses private, loopback, link-local and reserved destinations, and limits each client to 20 new
  connections per second and 256 distinct targets per minute (V4). `unreachable` covers refused,
  reset and timed out alike (Decision D19), so the exit is not a port scanner.
- Padding: exit streams use PADDING frames according to the exit address's shape profile, including on
  hidden exits (V2: v1 hidden exits did not pad at all).
- Codes: 0x100 `payment-required`, 0x101 `policy`.
- Later: vault contract v2 with code version and per-exit payment keys (K6, V5), MASQUE, resumable
  tunnels across paths.

## 8. files/2 (service 7)

File transfer between two nodes over their session (direct or relayed). The offer travels inside a
chat/2 message: file ID (16 bytes), size, Merkle root, chunk size 65536, name, MIME type, sender node
ID.

| Type | Frame | Body |
|---|---|---|
| 0x40 | WANT | `{ bytes fid = 1; repeated Range ranges = 2; }`, `Range { uint64 start = 1; uint32 count = 2; }`, ≤ 256 chunks per WANT |
| 0x41 | CHUNK | `{ bytes fid = 1; uint64 index = 2; repeated bytes proof = 3; bytes data = 4; }` |
| 0x42 | CANCEL | `{ bytes fid = 1; uint32 reason = 2; }` 1 cancelled, 2 declined, 3 unavailable |
| 0x43 | DONE | `{ bytes fid = 1; }` |

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

| Type | Frame | Body |
|---|---|---|
| 0x40 | SIGNAL | `{ bytes origin = 1; bytes body = 2; bytes sig = 3; }` |

- `body` is a `CallSignal` protobuf with the v1 actions as an enum (tree manifest, heartbeats, join,
  leave, stream announcements, subscriptions, NACK, feedback, …) and the v1 fields as typed fields.
- Signals forwarded beyond the first hop (manifests, announcements) are signed by the originator:
  `sig = Ed25519(origin key, "echofog/call2/signal" ‖ body)`, over the exact bytes (P8: v1 signed a
  re-serialised JSON). Hop-local signals (ping, NACK) are authenticated by the session and carry no
  signature.
- `CALL_SIGNAL` datagrams duplicate lossy signals (NACK, reports) for low latency.

### 9.2 Media packet v3

`CALL_AUDIO` / `CALL_VIDEO` datagram payload:

```
kind(u8) ‖ epoch(u8) ‖ sender(u8) ‖ stream(u8) ‖ seq(u32) ‖ ts(u32) ‖ frag_idx(u8) ‖ frag_count(u8) ‖ fragment
```

- `kind`: 1 audio, 2 video, 3 heartbeat, 4 screen audio, 6 video FEC. `sender` is the sender's index
  in the current tree manifest (replaces v1's 32-byte ID: −31 bytes per fragment), `stream` its stream
  index.
- A media unit is encrypted with AES-256-GCM under a **per-sender key** (P1):
  ```
  k_sender = HKDF-SHA256(ikm = epoch_secret, salt = empty,
                         info = "echofog/call2/sender-key" ‖ sender_node_id(32), L = 32)
  nonce    = stream(u8) ‖ kind(u8) ‖ 0x0000 ‖ seq(u32) ‖ 0x00000000
  aad      = the unit header (the fields before frag_idx)
  ```
  `nonce ‖ ciphertext ‖ tag` is split into at most 16 fragments.
- `epoch_secret` of epoch 0 is `HKDF-SHA256(ikm = room secret, info = "echofog/call2/epoch" ‖ u32(0))`.
  On every join, leave or kick the coordinator draws a new random epoch secret, sends it to each
  participant over its end-to-end session, and switches 2 s later; a kicked member never receives it.
- Replay windows mark a fragment only after its unit has been authenticated (P1: v1 marked before
  authentication, which let anyone "mute" a sender by pre-empting sequence numbers). Relays MAY forward
  fragments before the unit is complete, but MUST NOT mark them as seen until it authenticates.
- NACK retransmissions have a budget per child (at most 2× the child's recent receive rate); claimed
  relay capacity is capped by measured upload (P1).
- Later: MLS epochs (`mls`), SFrame-style headers, constant-rate shaping profile for calls (P9),
  WebRTC-shaped carrier for cross-border calls.
