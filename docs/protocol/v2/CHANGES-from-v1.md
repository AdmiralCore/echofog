# Changes from v1

v1 is protocol 101/102 as shipped in 0.2.x. v2 (200) is a clean break: a v2 node does not speak v1.
Each change names the v1 weakness it addresses; [security.md](security.md) §5 says which are fixed
fully, partly or not at all.

## 1. Changes

| # | Area | v1 | v2 | Why |
|---|---|---|---|---|
| 1 | Masking keys | noise key and REALITY token key derived from the public peer ID | random X25519 masking keys in signed cards; noise3 keys only in peer and invitation cards, never published; per-invitation keys for hidden nodes | K1 |
| 2 | Mesh noise | `[salt 8][XOR][tag 4]`, BLAKE2b keystream, fixed sizes | noise3: Elligator2 dirty ephemeral, selector keyed on the ECDH secret, ChaCha20-Poly1305, QUIC-style packet-number protection, shape profiles | K1, K2, N1 |
| 3 | REALITY token | AES-GCM with a key from the peer ID, bound to the ML-KEM share | ECDH with the masking key, bound to the whole ClientHello and the dialed address, persisted shared replay cache | K1, V1 |
| 4 | Probe handling | non-v1 packets reached quic-go (Version Negotiation); limits dropped or reset | everything unauthenticated or over a limit goes to the cover; no VN/Retry/reset before the token | V1 |
| 5 | Server answer | Go TLS defaults, certificate inflated to a random size | the cover's group, cipher, extension order, SCID length and exact certificate length are mirrored; sealed mode keeps PQ when the cover is classical | V1 |
| 6 | Identifying strings | `O=EchoFog`, ALPN `echo-fog-protocole`, default SNI `www.microsoft.com` | neutral certificates, `h3`/`h2`, no default SNI | K1, V1 |
| 7 | Carrier choice | "is an SNI known" | rules from signed card role, scope and country; servers never over noise3 across borders | K2, C12 |
| 8 | STUN | plaintext STUN from the QUIC port every 20 s | no plaintext STUN on carrier sockets; OBSERVED_ADDR with agreement rules | K2, audit 5-6 #1 |
| 9 | Handshake | custom Ed25519 exchange, `version` never read, M1 ≤ 4 KiB | Noise XKhfs/XXhfs with ML-KEM-768, suite name and responder ID in the transcript, 16 KiB messages | K3, K5, K10 |
| 10 | Hidden nodes | responder sent M2 to anyone claiming a known key; 10 000-HMAC pass scan | XK only, transcript-bound proof, `pass_id` lookup, uniform silent failure, personal passes, rendezvous IDs at relays | K5, V4 |
| 11 | Versioning | one number for all | L0–L7 versioned separately; features `u16`; services with versions; threshold-signed release manifest | K3 |
| 12 | Frames | `[u32 len][u8 type]`, `iota` types truncated at 255 | `varint type ‖ flags ‖ varint len`, explicit registry with maxima, IGNORABLE flag, GREASE | K3 |
| 13 | Unknown input | unknown frame types raised abuse scores | `UNSUPPORTED`, ignorable frames, no scoring, extension-safe objects | K3 |
| 14 | Muxes and padding | QUIC streams vs smux + kind byte + vision; tunnel padding on top | one frame format; native byte-stream mux; PADDING frames by profile | V6, V2 |
| 15 | Envelope per message | every message signed (~170–210 bytes) even on authenticated links | authentication by the session; signatures only for stored or forwarded objects | V3 |
| 16 | Node information | SNI, role and addresses as unsigned annotations, last writer wins | signed node cards in three scopes, highest `seq` wins, verbatim forwarding, `hs_key` ↔ node ID binding | K4 |
| 17 | Signed objects | ad-hoc layouts; exit advert signed over re-serialised JSON | one envelope; signature over exact bytes; canonical protobuf; label, alg, seq, expiry | K3, P4, V6 |
| 18 | DHT | two record kinds; first record wins; replies matched by message ID; addresses overwritten by anyone | signed records under quota; day-salted keys; all 20 closest over 3 disjoint paths, max `seq`; session-bound replies; card-only table entries; network diversity; reader memory | K3, K4 |
| 19 | PEX | unsigned third-party entries on every connection | signed public cards of nodes actually reached, lazy, rate-limited, new/tried tables | K4, V2 |
| 20 | Paths | one connection per peer; migration half-wired | sessions with paths, PATH_BIND without a new handshake, sealed relay paths with one counter per session | §2.2 of the v1 review, V7 |
| 21 | Relay circuits | relay reads inner types; unsigned open notices allow route hijack; refusals reveal presence | end-to-end XK inside circuits, sealed records, one circuit ID, uniform delayed refusals, rendezvous IDs | K5, K7, K8 |
| 22 | Hole punching | addresses to any peer, fixed 76-byte bursts | inside end-to-end sessions, targets confirmed by the relay or the card, random junk, PATH_BIND | K7, V2, V4 |
| 23 | Store-and-forward | random walk with HopLimit 20; quotas by previous hop; delete without receipt | two-hop forward/mailbox, queue IDs, FETCH/ACK, per-day quotas by node ID, PoW for public queues, duplicate suppression | K7, K9 |
| 24 | Chat framing | identities in clear to relays; text-prefix commands; no padding | sealed sender, typed control fields, padding buckets that fit a mailbox item | K3, K7 |
| 25 | Chat resets | any undecryptable replay deleted the session | silent drop; receiver starts a fresh PQXDH itself | K8 |
| 26 | PQ chats | KEM prekey optional, classic bundles accepted for 30 days | KEM prekey mandatory; classic init refused | K10, audit 7-8 P1 |
| 27 | Calls | one room key; replay marking before authentication; JSON signals readable by relays | per-unit sender signatures, per-join keys, rekey on membership change, mark after verification, signals over E2E sessions, 1-byte sender index | P1, P8 |
| 28 | Files | one key for all group members, handler without sender | session-bound transfers to named recipients | P7 |
| 29 | Exit | messenger identity visible to the exit; neighbours and payment addresses public; result codes leak port state; plaintext DNS; balance polling | per-exit client identity; offer in the card without those; pay data to served clients; uniform `unreachable`; DoH with rebinding check; pushed accounting; credit caps; UDP limits | K6, V2, V4, V5 |
| 30 | Timers | fixed values (10 s, 20 s, 5 s, 300 ms…) | ≥ ±20 % jitter on every periodic timer | V2 |
| 31 | Time units | seconds and nanoseconds in the same field | seconds everywhere; clock offsets learned from peers | V6 |
| 32 | Node ID | `SHA-256(pub)` | `SHA-256(akey)` (algorithm-tagged) | L7 agility |
| 33 | Bans | IP bans after 8 failed handshakes | no pre-authentication bans; puzzles scale with source pressure | (CGNAT collateral damage) |
| 34 | Dead code | gossip, discovery service, mDNS, migration, 13 dead message types | not specified; v1 numbers are not reused because v2 has its own registries | V7 |
| 35 | Identity | one Ed25519 key per user, also the network identity | account key (node ID) and per-device keys (device ID) with an account-signed device list | multi-device readiness |
| 36 | Exit speed | bandwidth probe: 12 MiB bursts on request | rating from real tunnel throughput and user-started tests | V4 |
| 37 | Call authentication | none per sender | batched sender signatures | P1 |

## 2. Removed without replacement

- The `TypeReady` byte, `network_id`, `CapPoolLink` and the 32-bit capability mask.
- `TypeExitDirectory` (replaced by pex/2 `GET{exit}`), `TypeObservedAddr` (replaced by
  OBSERVED_ADDR), `TypeBandwidthProbe` (not in v2; an amplification vector, V4), knock/reset text
  prefixes, the v1 bundle signature contexts, classic X3DH inits, `TransportMigrate*`, gossip.
- Name records in the DHT (defined format only, until the `names` service).

## 3. Decisions

Choices this specification made where the design draft was open or contradictory, or where the audit
required a choice. Each is stated where it applies.

| ID | Decision | Where |
|---|---|---|
| D1 | Node ID = `SHA-256(akey)` of the account key (algorithm-tagged), not `SHA-256(pub)`; everyone gets a new ID once (accepted, OQ2) | README §5.2 |
| D2 | SHA-256 family for L0, L2, objects, links and services; BLAKE2s only inside Noise | wire §2.3 |
| D3 | The full suite string including `EchoFog/2/` is the Noise `protocol_name` | handshake §1 |
| D4 | `offered` = the initiator's suites ∩ the card's suites; responder enforces its best common suite; one retry after `REJECT{suite}` with the full list | handshake §3.1 |
| D5 | (revised) Bulk data rides `X25519MLKEM768` TLS where the carrier has it, otherwise it is sealed with the hybrid Noise keys; REALITY mirrors the cover's group; carrier early data forbidden | handshake §3.2, wire §3.4, §8.5 |
| D6 | The exit offer is part of the card, not a separate object | objects §4 |
| D7 | The signature covers the whole envelope prefix | objects §2.1 |
| D8 | `seq` and `not_after` are envelope fields | objects §2.1 |
| D9 | One DHT record per (label, signer); no sub-keys | dht §2 |
| D10 | (revised) ≤ 8 live masking keys per listener for both noise3 and REALITY; more listeners for more keys; stranger budgets | wire §2.1 |
| D11 | noise3 has no forward secrecy at L0; a node that may have lost `mask_sk` rotates masking and `hs` keys | wire §2.3 |
| D12 | Card lifetime ≤ 7 days; invitation cards ≤ 30 days; masking keys ≤ 7 days beyond their card | objects §3.2, §10.3 |
| D13 | (revised, OQ7) Chat PQXDH/ratchet constructions kept; labels renamed to `echofog/chat2/*` and `echofog/storage2/key`; all `FOG_*` labels retired | services §4.0, registries §7.5–§7.6 |
| D14 | Native byte-stream mux replaces smux, the stream-kind byte and vision | wire §5.2 |
| D15 | Full XKhfs/XXhfs transcript vectors are deferred; two independent implementations must agree before L1 is final | handshake §12 |
| D16 | (revised, OQ3) The maintainer holds 3 release keys on separate offline media; a manifest needs 2 of 3 signatures; threshold rotation and revocation; `seq = released_at`; never-lock-out rules; fixed DHT key; clients only notify, never install | objects §9, versioning §5 |
| D17 | Shop links: `https://` or `echofog://contact` only; punycode domain, no look-alikes, off-vault warning, shown after 14 days and a delivery-based rating | objects §3.5 |
| D18 | GREASE in features, L1 suites, services and frame types; values `0xXAXA`, X in 0..e | registries §5 |
| D19 | exit/2 does not distinguish refused, reset and timed-out destinations (it still reveals open ports) | registries §10.4 |
| D20 | No plaintext STUN on carrier sockets; public address needs 2 agreeing peers in different /16s; no tie-breaking | wire §7.2 |
| D21 | Servers and cross-border links use camouflaged suites; noise3 only with positive same-country knowledge | wire §1 |
| D22 | Hidden nodes refuse XX silently; XX to a bare address only under a throwaway identity | handshake §4.2 |
| D23 | Card scopes public / peer / invite; noise3 addresses and keys never in public cards | objects §3.2, wire §2.1 |
| D24 | noise3 selector keyed on the ECDH secret | wire §2.3 |
| D25 | noise3 packet numbers protected as in QUIC (32-bit truncation, ChaCha20 mask) | wire §2.6 |
| D26 | DHT keys salted with the day; diversity unit IPv4 /24, IPv6 /32 | dht §1–§2 |
| D27 | Name records are not stored in the v2 DHT | dht §2 |
| D28 | Unlisted nodes are reachable at relays only by per-contact daily rendezvous IDs; uniform delayed refusals | services §6.3 |
| D29 | Clients use a per-exit identity towards exits | services §7 |
| D30 | Call media units are signed per unit by the sender's per-call key; media keys are per join | services §9.2 |
| D31 | L0 timestamp windows ±300 s; clock offsets learned from authenticated peers; handshake skew limit 600 s | wire §2.5, §3.3, handshake §7 |
| D32 | No pre-authentication IP bans; per-source pressure raises puzzle difficulty | handshake §11 |
| D33 | Hidden nodes share one rotated contacts masking key among redeemed contacts | objects §10.3 |
| D34 | (OQ6) Proof of work (default 20 bits) is the first-contact gate at launch; payment and tokens later | services §5.2 |
| D35 | (OQ1) Separate account key (from the mnemonic; node ID) and device keys (random; device ID); the account signs the device list, which every card embeds | README §5, objects §1, §3, §6, handshake §3.3, dht §1–§2 |
| D36 | (OQ9) Clock check: majority of `Date` headers from several large CDN endpoints, defaults in registries §12, configurable | handshake §7, registries §12 |
| D37 | (OQ4) Every stranger-reachable server runs REALITY (or ws2) with a cover; the installer finds the cover automatically | wire §2.1, §3.5 |
| D38 | (OQ8) Exit speed rating from real tunnel throughput plus a user-started test; no bursts on request | services §7.1 |
| D39 | (OQ10) Call media authenticated by batch signatures (≤ 100 ms batches, per-unit hashes, overlap with the previous batch) | services §9.3, registries §2 |
| D41 | The core builds with `gomobile bind` (android/arm64, android/amd64, ios/arm64), without cgo, with build-tagged platform code and portable fallbacks; platform differences are a runtime profile and capability flags that never change the wire | platforms.md |
| D40 | (OQ5) Ship with the current vault; cross-exit payment linkage is a residual risk; vault contract v2 on the payments roadmap | services §7, security §4.8 |

## 4. Open questions

All open questions of the audited draft were answered by the user (November 2026) and are now
Decisions:

| ID | Answer | Decision |
|---|---|---|
| OQ1 | separate account and device keys already in v2 | D35 |
| OQ2 | accept new node IDs for everyone, consistent with D35 | D1 |
| OQ3 | maintainer holds 3 keys on separate media; 2 of 3; notify only | D16 |
| OQ4 | every stranger-reachable server needs a REALITY cover; automatic discovery | D37 |
| OQ5 | ship with the current vault linkage as a residual risk; vault v2 on the roadmap | D40 |
| OQ6 | proof of work (20 bits) at launch; payment and tokens later | D34 |
| OQ7 | rename the chat KDF labels now | D13 |
| OQ8 | exit speed rating from real traffic and a manual test | D38 |
| OQ9 | majority of several CDN `Date` headers, configurable | D36 |
| OQ10 | batch the call signatures | D39 |

No open questions remain in this specification.

## 5. Audit of the first draft: how each finding was handled

Section numbers refer to the audit report (`audit-spec-v2.md`, November 2026).

| Audit § | Severity | Finding | Status | Where |
|---|---|---|---|---|
| 1.1 | critical | noise3 selector on the public key: passive recognition | fixed: selector keyed on `z`; noise3 keys never published | wire §2.1, §2.3; objects §3.2 |
| 1.2 | high | noise3 window: flow dies after 65 lost packets | fixed: QUIC-style packet-number protection, dead-flow restart | wire §2.6 |
| 1.3 | medium | reflection, pending-flow eviction, reply redirection | fixed: 3× limit, Retry, random eviction, two-packet address change | wire §2.5, §2.6 |
| 1.4 | low | cost of D11 | fixed: rotate masking and `hs` keys after a possible theft | wire §2.3; security §4.5 |
| 1.5 | low | first datagram shape, inner Initial size, MTU | partly: size and MTU rules fixed; QUIC-shaped Init deferred (needs research) | wire §2.4, §2.7; security §4.4 |
| 1.6 | low | which pending flow answers; source address | fixed | wire §2.5 step 6 |
| 1.7 | — | Elligator2: tweak source, vector without representative | fixed | wire §2.2; elligator2.json |
| 2.1 | high | ServerHello differs from the cover's | fixed: cover profile mirrored; sealed mode for classical covers | wire §3.4; handshake §3.2 |
| 2.2 | medium | intercept-and-replay confirms a node | partly: exact certificate length, record-size copying; residual stated | wire §3.4; security §4.2 |
| 2.3 | medium | gateway overload distinguishable; token not bound to address | fixed | wire §3.2, §3.3 |
| 2.4 | low | ws2 path and behaviour | fixed | wire §4.4 |
| 3.1 | medium | unknown key-share | fixed: responder ID in the prologue; `hs_key` binding | handshake §3.3, §7 |
| 3.2 | medium | XX bootstrap MITM | fixed: expected IDs required; throwaway identity otherwise | handshake §4.2; wire §1 |
| 3.3 | medium | hidden-node timing oracle | fixed: uniform failure timing | handshake §5 |
| 3.4 | medium | re-entry, group revocation, unlisted card after a session | fixed: personal passes, peer cards, contacts key, revocation rules | handshake §5; objects §10.3 |
| 3.5 | medium | hidden nodes cannot upgrade L1 | fixed: `offered` limited to the card | handshake §3.1 |
| 3.6 | medium | puzzle order | fixed: puzzle before message 1 processing; S21 reworded | handshake §5, §6; security S21 |
| 3.7 | medium | `hs_key` rotation | fixed: `prev_hs` | handshake §7.1; objects §3.1 |
| 3.8 | medium | IP bans under CGNAT | fixed: no pre-auth bans | handshake §11 |
| 3.9 | low | REJECT detail format, retry list, higher `proto_ver`, early streams, GREASE order | fixed; `pass_id` on relays under a quantum adversary listed as residual | handshake §3.1, §3.4, §7; wire §6.2; security §4.7 |
| 4.1 | high | release manifest as a kill switch | fixed: 2-of-3 threshold, revocation, bounded `seq`, never-lock-out, fixed DHT key, notify-only (D16) | objects §9; versioning §5; dht §2 |
| 4.2 | medium | objects closed to extension | fixed | objects §2.2, §2.3, §3.1; registries §11 |
| 4.3 | low | GREASE only for features; overlap with experimental range | fixed | registries §5; handshake §3.4 |
| 4.4 | low | equivocation rules; invitation cards | fixed: one rule; scopes and `invite_id` | objects §2.2, §3.2 |
| 4.5 | low | empty embedded messages | fixed | objects §2.3 |
| 5.1 | high | DHT value lookup stops after 3 answers | fixed as far as a DHT can: all 20 closest, 3 disjoint paths, day salt, diversity, reader memory; S27/S28 and residual 9 state the limit | dht §2, §4; security |
| 5.2 | medium | name squatting | fixed: not stored in v2 | dht §2 |
| 5.3 | medium | reflection through cards, FORWARD, PUNCH_OFFER | fixed | dht §4.3; services §5.3, §6.4 |
| 5.4 | low | quotas, IPv6, guards, NODES size, stop rule | fixed | dht §1, §3, §4 |
| 5.5 | low | shop links | fixed | objects §3.5 |
| 5.6 | low | multi-device readiness | fixed: separate account and device keys (D35) | objects §1, §3.4, §6 |
| 6.1 | high | call sender keys derivable by any member | fixed: per-unit signatures; S35 rewritten | services §9.2 |
| 6.2 | high | relay presence oracle | fixed: uniform refusals, rendezvous IDs | services §6.3 |
| 6.3 | medium | mailbox quota exhaustion | fixed: per-day node-ID quotas, PoW (D34), dedupe | services §5 |
| 6.4 | medium | exit sees the client identity; vault links exits | partly: per-exit identity; vault linkability accepted as residual (D40) | services §7 |
| 6.5 | medium | `circuit_id` ambiguity; sealed counters with several paths | fixed | services §6.2; handshake §3.2; wire §8.4 |
| 6.6 | medium | service frame maxima; chat envelope vs mailbox item | fixed | services (Max columns); registries §4; services §4.3 |
| 6.7 | medium | hole punching unspecified | fixed (symmetric NAT reserved) | services §6.4 |
| 6.8 | medium | call nonce reuse on rejoin | fixed: per-join keys | services §9.2 |
| 6.9 | low | service timeouts, exit UDP, rebinding, D19 claim, `h`/`sid`, guard lookups | fixed | services; handshake §9; security §4.8 |
| 7 #2 | medium | L0 clock windows narrower than the handshake's | fixed: ±300 s and learned offsets | wire §2.5, §3.3; handshake §7 |
| 7 #3–#5, #16, #17 | low | `FOG_X3DH_V2`, domestic-only flag, unknown frame on stream 0, timer wording, higher `proto_ver` | fixed | registries §7.6, §11; wire §6.2, §7.1; handshake §7 |
| 8.1 | medium | no handshake vectors | deferred to step 2 with an exit criterion of two independent implementations | handshake §12 |
| 8.2 | low | vector gaps | partly: negative vectors, real cookie, full sealed envelope and mailbox seal, hybrid-share example, card scopes added; a real Chrome ClientHello split over two Initials and a multi-stream MUX example remain to do | vectors/ |
