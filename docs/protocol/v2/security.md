# Security properties

Each property names the adversary of [README.md](README.md) §2 it holds against, the attack it
defeats, the mechanism, and the v1 weaknesses it closes. §4 lists what remains.

## 1. Carriers (L0)

| # | Property | Adversary / attack defeated | Mechanism | Closes |
|---|---|---|---|---|
| S1 | A noise3 flow is indistinguishable from uniformly random UDP to anyone without the masking public key | A1 recognises flows by recomputing keys from public node IDs | masking keys are random, not derived from the ID, and only in cards; Elligator2 representative of a dirty ephemeral key; every byte after it is MAC or AEAD output; packet numbers are hidden behind per-packet tags | K1, K2, N1 |
| S2 | Knowing a node's ID, card-less, gives no way to confirm it | A2 builds a valid first packet or token from the node ID | first packets need `mask_pub`; ECDH-based token for REALITY v2 | K1 |
| S3 | No answer to unauthenticated input | A2 sends garbage, other QUIC versions, replays | noise3 drops silently; REALITY forwards everything unauthenticated to the cover, sends no Version Negotiation, no Retry and no reset before the token check | V1 |
| S4 | Replays are useless within and across restarts | A2 replays a captured first packet or ClientHello to get the node's own answer | timestamps (±120 s / ±90 s), replay caches persisted as Bloom filters, token bound to the whole ClientHello and to the server key | V1, N1 |
| S5 | A captured REALITY token cannot be moved into another ClientHello | A2 copies `random` into a hello with its own key shares | token key from `X25519(c, mask_pub)` and AAD over the full ClientHello | V1 |
| S6 | Variable first-packet length and shape | A1 classifies by "1292-byte first datagram", 76-byte punch bursts | shape profiles as data: Init length distribution, padding, junk and decoys | K2, V2 |
| S7 | No plaintext STUN from carrier sockets | A1 links "STUN to Google every 20 s" with "random UDP to a VPS" on the same port | address discovery from authenticated peers (OBSERVED_ADDR) with agreement rules | K2, audit 5-6 #1 |
| S8 | Servers are never dialed across a border over noise3 | A1 sees random UDP to data-centre IPs | role and suites only from signed cards; unknown country counts as border | K2, S12 |
| S9 | Neutral identifiers | A1/A2 read `O=EchoFog`, ALPN `echo-fog-protocole` | neutral certificates, ALPN `h3`/`h2` only | K1 |
| S10 | One padding mechanism per carrier, budgets randomised per stream | A1 learns "always exactly 4096 padded bytes" or the double-padded TCP shape | PADDING frames governed by profiles; no separate vision or tunnel padding | V2, V6 |

## 2. Handshake and session (L1, L2)

| # | Property | Adversary / attack defeated | Mechanism | Closes |
|---|---|---|---|---|
| S11 | Mutual authentication of node identities | A4 impersonates a node | Noise XK/XX; static keys bound to identities by signed cards; the remote static must equal the card's `hs_key` | — |
| S12 | Binding to the carrier TLS | A4 or A1 terminates TLS on both sides and relays the handshake | `cb` = TLS exporter in the prologue; PATH_BIND MAC covers the new path's `cb` | — |
| S13 | Hybrid post-quantum key agreement | A5 records handshakes now | `ekem1` with ML-KEM-768 mixed into the keys before the initiator's identity and the session data | K10 |
| S14 | Hidden responders reveal nothing before the invitation is proven | A2/A3 knows a contact's key or the node ID and probes for the hidden node | per-invitation masking keys; XK only; access proof bound to the transcript; nothing is sent before the proof verifies; silent close with a random delay | K5 |
| S15 | The initiator reveals its identity only to the intended responder | anyone on the address learns who calls | identity in message 3, after message 2 proved the card's `hs` key | K5 |
| S16 | No downgrade of suites, features or services | A1/A4 strips options | protocol name, HS_HELLO (offered list) and payload lists in the transcript; responder enforces its best common suite | K3 |
| S17 | Admission decisions use proven identities only | A4 claims a known peer's key to skip PoW or limits | identity known only after message 3; puzzle before any expensive work; pass lookup by `pass_id`, no linear scan | K5, V4 |
| S18 | 0-RTT data is limited to public, idempotent queries and replay-bounded | A1 replays message 1 to repeat a request | XK only, time window, replay cache keyed by the ephemeral key, allow-list of operations | V3 |
| S19 | Explained refusals, no silent version failures, no abuse score for the unknown | v1: new frame types got peers banned; mixed versions failed by timeout | REJECT/UNSUPPORTED inside the channel; unknown frames, fields and services never scored | K3 |

## 3. Objects, DHT, services

| # | Property | Adversary / attack defeated | Mechanism | Closes |
|---|---|---|---|---|
| S20 | Node information cannot be forged or overwritten by third parties | A3/A4 injects SNI markers, `srv=1` flags or addresses through PEX and DHT replies | only signed cards carry addresses, roles, SNI and masking keys; highest `seq` wins; verbatim forwarding | K4 |
| S21 | No rollback of records | A3 serves an old card, bundle or price | value lookups collect ≥ 3 answers from different /24s and take the highest `seq`; stores refuse lower `seq` | K4, audit 7-8 P1 |
| S22 | Replies are bound to the asked node | A3 answers a lookup it was not asked | replies arrive on the authenticated session with the asked node | K4 |
| S23 | Eclipse resistance of the routing table | A3 fills buckets from one subnet, rewrites addresses | entries only from sessions or signed cards; ≤ 2 per /24 per bucket; disjoint lookups for sensitive keys | K4 |
| S24 | One encoding per object | A4 re-encodes or pads a signed object | signatures over exact bytes; canonical protobuf required; equivocation rule | K3, P4 |
| S25 | Sealed sender and padded messages | A4 (relay, mailbox) reads sender, receipt shapes, lengths | SealedEnvelope; plaintext padded to buckets; typed control fields | K7 |
| S26 | Two-hop mailbox delivery with receipts | A4 mailbox learns sender and recipient; push-and-delete loses mail | forwarding relay ≠ mailbox relay; queue IDs instead of peer IDs; FETCH/ACK; one hop, no random walk; quotas per actual session | K7, K9 |
| S27 | Relay circuits are end-to-end protected and cannot be hijacked | A4 relay reads inner types; a neighbour claims to be the source | end-to-end XK inside the circuit; sealed records; refusals name the target | K7, K8 |
| S28 | Chat sessions are not broken by replays | A4 replays an old message to force a reset | silent drop of replays; no resets on failure; fresh PQXDH started by the receiver itself | K8 |
| S29 | No classic-only chat sessions | A3 replays a pre-upgrade bundle | every v2 bundle has a KEM prekey; classic init refused | K10, audit 7-8 P1 |
| S30 | Per-sender call keys, rekey on membership change, authentication before replay marking | A4 call member forges another's media or "mutes" a sender | HKDF per sender; new epoch on join/leave/kick; mark after authentication; NACK budgets | P1 |
| S31 | Call signals hidden from relays and signed over exact bytes | A4 relay reads the tree, VAD and NAT types | signalling only over end-to-end sessions; originator signature over bytes | P8 |
| S32 | File transfers bound to the authenticated peer | A4 group member cancels or redirects another member's download | session-bound transfers; sender serves only named recipients | P7 |
| S33 | Exit leaks less | A4 exit links payments; A1 scans through exits; A3 harvests payment data | no neighbours or pay addresses in offers; random pay tags; DoH at the exit; uniform `unreachable`; rate limits; unpaid-credit cap; price checked against the signed offer | K6, V4, V5 |
| S34 | Hole-punch addresses only to authenticated peers | A3 asks any node for its public, IPv6 and LAN addresses | coordination inside an end-to-end session; LAN only on matching public IP | K7 |
| S35 | Bounded amplification | A4 turns small requests into large answers | per-type frame limits (≤ 1 MiB), PEX/DHT rate limits, proxied-lookup buckets, no bandwidth probe service in v2, NACK budgets | V4 |

## 4. Residual risks

1. **Public nodes are confirmable.** Anyone can obtain a public node's card (that is what public
   means) and so its masking key, and can then build valid first packets and confirm the node. Only
   hidden nodes and per-invitation keys avoid this. Operators of entry nodes in censored regions
   should prefer hidden or invite-only nodes.
2. **Bulk data on direct paths relies on the carrier TLS** (Decision D5). Its post-quantum
   confidentiality holds only because both sides insist on `X25519MLKEM768`; a TLS-library bug in the
   group check would silently weaken it.
3. **noise3 has no L0 forward secrecy** (Decision D11). A stolen masking key exposes the inner QUIC
   packets of recorded flows (still TLS-protected) and makes those flows recognisable after the fact.
4. **Elligator2 hides the key, not the behaviour.** Flow timing, packet sizes beyond the profile's
   range, connection counts and data-centre destinations remain observable; censors that block all
   unrecognised UDP (Iran in crises) or freeze flows to data-centre IPs after ~16 KB (Russia) are not
   defeated by masking. Domestic entry nodes, guards and REALITY over TCP remain the answer.
5. **REALITY covers are imitated, not identical.** Post-handshake record sizes, certificate chain,
   session tickets and HTTP/3 behaviour of the node can differ from the cover site; the size matching
   rules reduce but do not remove this.
6. **Signatures are classical.** Ed25519 identities are forgeable by a future quantum adversary in real
   time; algorithm tags make the move to hybrid signatures possible but v2 does not make it.
7. **Sealed sender is classical.** The sealed-sender layer uses X25519 only; a future quantum
   adversary can unseal recorded envelopes and learn senders (not contents, which are protected by
   PQXDH and the ratchet). The ratchet itself is not post-quantum after the first message until SPQR.
8. **Metadata at relays and exits.** A relay sees both ends of a circuit, sizes and timing; a mailbox
   relay sees the forwarder; an exit sees the client's node ID, all destinations and volumes. EchoFog
   is not a mixnet.
9. **Sybil resistance is economic, not cryptographic.** Node IDs are free; the DHT defences
   (diversity, multi-answer lookups, signed cards) raise the cost of eclipse attacks but do not stop a
   well-resourced adversary with many subnets.
10. **0-RTT data** is not forward-secret against theft of the responder's handshake key and is
    replayable once per responder within 60 s; only public queries are allowed in it.
11. **Clock dependence.** Cards, tokens, Init timestamps and 0-RTT need clocks within ±90 s to ±300 s;
    a node with a badly wrong clock cannot connect until it corrects it (it learns the skew from
    `REJECT{clock-skew}` from public nodes).
12. **Group membership and convlog semantics** (P2–P6, P10) are application-level and are not fixed by
    the transport changes of v2; chat/2 carries the group log unchanged. They need the separate groups
    work (single source of membership, rules-version events, Negentropy sync, bound group IDs).
13. **Hidden-node invitations are bearer secrets.** Whoever holds a link (secret and card) can
    connect until it is revoked; v2 limits linkage between invitations but not the leak of one.
