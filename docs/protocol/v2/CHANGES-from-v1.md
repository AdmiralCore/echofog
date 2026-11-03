# Changes from v1

v1 is protocol 101/102 as shipped in 0.2.x. v2 (200) is a clean break: a v2 node does not speak v1.
Each change names the v1 weakness it addresses.

## 1. Changes

| # | Area | v1 | v2 | Why |
|---|---|---|---|---|
| 1 | Masking keys | noise key and REALITY token key derived from the public peer ID | random X25519 masking keys in the signed card; per-invitation keys for hidden nodes | K1 |
| 2 | Mesh noise | `[salt 8][XOR][tag 4]`, BLAKE2b keystream, fixed sizes | noise3: Elligator2 dirty ephemeral, selector MAC, ChaCha20-Poly1305, hidden packet numbers, shape profiles | K1, K2, N1 |
| 3 | REALITY token | AES-GCM with a key from the peer ID, bound to the ML-KEM share | ECDH with the masking key, bound to the whole ClientHello, persisted replay cache, ±90 s | K1, V1 |
| 4 | Probe handling | non-v1 packets reached quic-go (Version Negotiation) | everything unauthenticated goes to the cover; no VN/Retry/reset before the token | V1 |
| 5 | Identifying strings | `O=EchoFog`, ALPN `echo-fog-protocole`, default SNI `www.microsoft.com` | neutral certificates, `h3`/`h2`, no default SNI | K1, V1 |
| 6 | Carrier choice | "is an SNI known" | rules from signed card role and country; servers never over noise3 across borders | K2, S12 |
| 7 | STUN | plaintext STUN from the QUIC port every 20 s | no plaintext STUN on carrier sockets; OBSERVED_ADDR with agreement rules | K2, audit 5-6 #1 |
| 8 | Handshake | custom Ed25519 exchange, `version` field never read, M1 ≤ 4 KiB | Noise XKhfs/XXhfs with ML-KEM-768, suite name in transcript, 16 KiB messages | K3, K5, K10 |
| 9 | Hidden nodes | responder sent M2 to anyone claiming a known key; 10 000-HMAC pass scan | XK only, transcript-bound access proof, `pass_id` lookup, silence before proof | K5, V4 |
| 10 | Versioning | one number for all | L0–L7 versioned separately; features `u16`; services with versions; release manifest | K3 |
| 11 | Frames | `[u32 len][u8 type]`, `iota` types truncated at 255 | `varint type ‖ flags ‖ varint len`, explicit registry, IGNORABLE flag | K3 |
| 12 | Unknown input | unknown frame types raised abuse scores | `UNSUPPORTED`, ignorable frames, no scoring | K3 |
| 13 | Muxes and padding | QUIC streams vs smux + kind byte + vision; tunnel padding on top | one frame format; native byte-stream mux; PADDING frames by profile | V6, V2 |
| 14 | Envelope per message | every message signed (~170–210 bytes) even on authenticated links | authentication by the session; signatures only for stored or forwarded objects | V3 |
| 15 | Node information | SNI, role and addresses as unsigned annotations, last writer wins | signed node card, highest `seq` wins, verbatim forwarding | K4 |
| 16 | Signed objects | ad-hoc layouts; exit advert signed over re-serialised JSON | one envelope; signature over exact bytes; canonical protobuf; label, alg, seq, expiry | K3, P4, V6 |
| 17 | DHT | two record kinds; first record wins; replies matched by message ID; addresses overwritten by anyone | any signed kind under quota; ≥ 3 answers, max `seq`; session-bound replies; card-only table entries; subnet diversity | K3, K4 |
| 18 | PEX | unsigned third-party entries on every connection | signed cards only, lazy, rate-limited, new/tried tables | K4, V2 |
| 19 | Paths | one connection per peer; migration half-wired | sessions with paths, PATH_BIND without a new handshake, sealed relay paths | §2.2 of the v1 review, V7 |
| 20 | Relay circuits | relay reads inner types; unsigned open notices allow route hijack | end-to-end XK inside circuits, sealed records, refusals name the target | K7, K8 |
| 21 | Store-and-forward | random walk with HopLimit 20; quotas by previous hop; delete without receipt | two-hop forward/mailbox, queue IDs, FETCH/ACK, one hop | K7, K9 |
| 22 | Chat framing | headers and identities in clear to relays; text-prefix commands; no padding | sealed sender, typed control fields, padding buckets | K3, K7 |
| 23 | Chat resets | any undecryptable replay deleted the session | silent drop; receiver starts a fresh PQXDH itself | K8 |
| 24 | PQ chats | KEM prekey optional, classic bundles accepted for 30 days | KEM prekey mandatory in `prekeys/2`; classic init refused | K10, audit 7-8 P1 |
| 25 | Calls | one room key; replay marking before authentication; JSON signals readable by relays | per-sender keys, rekey on membership change, mark after authentication, signals over E2E sessions, 1-byte sender index | P1, P8 |
| 26 | Files | one key for all group members, handler without sender | session-bound transfers to named recipients | P7 |
| 27 | Exit | neighbours and payment addresses public; result codes leak port state; plaintext DNS at the exit; balance polling | offer in the card without those; pay data to served clients; uniform `unreachable`; DoH; pushed accounting; credit caps | K6, V2, V4, V5 |
| 28 | Timers | fixed values (10 s, 20 s, 5 s, 300 ms…) | ≥ ±20 % jitter everywhere | V2 |
| 29 | Time units | seconds and nanoseconds in the same field | seconds everywhere | V6 |
| 30 | Node ID | `SHA-256(pub)` | `SHA-256(akey)` (algorithm-tagged) | L7 agility |
| 31 | Dead code | gossip, discovery service, mDNS, migration, 13 dead message types | not specified; numbers of v1 are not reused because v2 has its own registries | V7 |

## 2. Removed without replacement

- The `TypeReady` byte, `network_id`, `CapPoolLink` and the 32-bit capability mask.
- `TypeExitDirectory` (replaced by pex/2 `GET{exit}`), `TypeObservedAddr` (replaced by
  OBSERVED_ADDR), `TypeBandwidthProbe` (not in v2; an amplification vector, V4), knock/reset text
  prefixes, the v1 bundle signature contexts, `TransportMigrate*`, gossip.

## 3. Decisions

Choices this specification made where the design draft was open or contradictory. Each is stated where
it applies.

| ID | Decision | Where |
|---|---|---|
| D1 | Node ID = `SHA-256(akey)` (algorithm-tagged key), not `SHA-256(pub)` | README §5.2 |
| D2 | SHA-256 family for L0, L2, objects, links and services; BLAKE2s only inside Noise | wire §2.3 |
| D3 | The full suite string including `EchoFog/2/` is the Noise `protocol_name` | handshake §1 |
| D4 | Initiator chooses the suite from the card; offered list in HS_HELLO and prologue; responder enforces its best common suite; one retry after `REJECT{suite}` | handshake §3.1 |
| D5 | On direct paths the carrier TLS carries the data, bound by exporter; TLS MUST use X25519MLKEM768; carrier early data forbidden; relay paths sealed with Noise keys | handshake §3.2 |
| D6 | The exit offer is part of the card, not a separate object | objects §4 |
| D7 | The signature covers the whole envelope prefix (length-prefixed label, alg, key, seq, expiry, body) | objects §2.1 |
| D8 | `seq` and `not_after` are envelope fields | objects §2.1 |
| D9 | One DHT record per (label, signer); no sub-keys | dht §2 |
| D10 | ≤ 256 masking keys per noise3 listener, ≤ 8 per REALITY listener; invitations beyond that share keys in groups | wire §2.1 |
| D11 | noise3 has no forward secrecy at L0 | wire §2.3 |
| D12 | Card lifetime ≤ 7 days; invitation cards ≤ 30 days, never published | objects §3.2, §10.3 |
| D13 | Chat PQXDH/ratchet constructions and their v1 labels are kept unchanged | registries §7.6 |
| D14 | Native byte-stream mux replaces smux, the stream-kind byte and vision | wire §5.2 |
| D15 | Full XKhfs/XXhfs transcript vectors are deferred to the step-2 implementation | handshake §12 |
| D16 | Up to 3 compiled-in release keys; rotation through `next_keys` | objects §9 |
| D17 | Shop links: `https://` or `echofog://contact` only; shown with a label, after 14 days and a rating threshold, never auto-opened | objects §3.5 |
| D18 | GREASE feature IDs `0xXAXA` | registries §5 |
| D19 | exit/2 does not distinguish refused, reset and timed-out destinations | registries §10.3 |
| D20 | No plaintext STUN on carrier sockets; public address needs 2 agreeing peers in different /16s; no tie-breaking | wire §7.2 |
| D21 | Carrier selection: servers and cross-border links use camouflaged suites; noise3 only with positive same-country knowledge from an offline GeoIP database | wire §1 |
| D22 | Hidden nodes refuse XX silently | handshake §4.2 |
