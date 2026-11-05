# Security properties

Each property names the adversary of [README.md](README.md) §2 it holds against, the attack it
defeats, the mechanism, and the v1 weaknesses it addresses. §4 lists what remains; §5 states, for
every v1 weakness, whether v2 fixes it fully, partly or not at all. Claims were revised after the
independent audit of the first draft ([CHANGES-from-v1.md](CHANGES-from-v1.md) §5).

## 1. Carriers (L0)

| # | Property | Adversary / attack defeated | Mechanism | Addresses |
|---|---|---|---|---|
| S1 | A passive observer cannot recognise a noise3 flow, even knowing the node's identity, cards and masking public key | A1+A3 crawls the DHT and tests first datagrams against published keys | selector and keys derived from the ECDH secret `z`; noise3 addresses and keys only in peer and invitation cards, never published; Elligator2 representative of a dirty ephemeral key; packet numbers hidden by header protection | K1, K2, N1 |
| S2 | Knowing a node ID gives no way to build a valid first packet | A2 builds first packets or tokens from public IDs (v1) | first packets need a masking key, which is random and not derived from the ID | K1 |
| S3 | No answer to unauthenticated input | A2 sends garbage, other QUIC versions, replays | noise3 drops silently; REALITY forwards everything unauthenticated, and everything beyond a limit, to the cover; no Version Negotiation, Retry or reset before the token check | V1 |
| S4 | Exact replays are useless, also across restarts and across addresses | A2 replays a captured first packet or ClientHello | timestamps (±300 s), persisted replay caches shared by all listeners of a key, token bound to the whole ClientHello, the server key and the dialed address | V1, N1 |
| S5 | A captured REALITY token cannot be moved into another ClientHello | A2 copies `random` into a hello with its own key shares | token key from `X25519(c, mask_pub)` and AAD over the full ClientHello | V1 |
| S6 | The node's REALITY server answers like the cover up to the end of the handshake | A1 compares the ServerHello (group, cipher, SCID length, certificate size) with the cover's | cover profile measured periodically and reproduced exactly; suite disabled when it cannot be; post-quantum confidentiality kept by sealed mode when the cover is classical | V1 |
| S7 | Variable first-packet length and shape, no fixed sizes | A1 classifies by "1292-byte first datagram", 76-byte punch bursts | shape profiles as data: Init length distribution, padding, junk, decoys; random punch packets | K2, V2 (partly, §4.4) |
| S8 | No plaintext STUN from carrier sockets | A1 links "STUN to Google every 20 s" with "random UDP" on the same port | OBSERVED_ADDR from authenticated peers with agreement rules | K2, audit 5-6 #1 |
| S9 | Servers are never dialed across a border over noise3 | A1 sees random UDP to data-centre IPs | role and suites only from signed cards; unknown country counts as border | K2, C12 |
| S10 | Neutral identifiers | A1/A2 read `O=EchoFog`, ALPN `echo-fog-protocole` | neutral certificates, ALPN `h3`/`h2` | K1 |
| S11 | One padding mechanism per carrier, randomised budgets | A1 learns "exactly 4096 padded bytes" or the double-padded TCP shape | PADDING frames governed by profiles | V2, V6 |
| S12 | No loss-induced flow death; no cheap redirection | v1/draft: 65 lost packets killed a flow; an on-path racer could redirect replies | 32-bit packet-number reconstruction (±2^31); reply address moves only after 2 fresh packets; dead-flow restart | — |
| S13 | Bounded reflection through noise3 | A4 with a masking key spoofs Inits to make the node blast a victim | 3× anti-amplification until the inner handshake confirms; Retry under load; random eviction of pending flows; noise3 keys known only to peers | V4 |

## 2. Handshake and session (L1, L2)

| # | Property | Adversary / attack defeated | Mechanism | Addresses |
|---|---|---|---|---|
| S14 | Mutual authentication of node identities | A4 impersonates a node | Noise XK/XX; static keys bound to identities by signed cards; the remote static must equal the card's `hs_key` | — |
| S15 | No unknown key-share | A4 publishes a card with another node's `hs_key` to borrow its answers | the expected responder device ID is in the prologue; `hs_key` ↔ node ID bindings remembered, conflicting cards rejected | K4 |
| S16 | Binding to the carrier TLS when the responder is known | A4/A1 terminates TLS on both sides and relays the handshake | `cb` = TLS exporter in the prologue (XK, or XX with an expected ID); PATH_BIND MAC covers the new path's `cb`. Not for XX to a bare address, which uses a throwaway identity (§4.10) | — |
| S17 | Hybrid post-quantum key agreement, and post-quantum bulk data on every path | A5 records handshakes and traffic | `ekem1` with ML-KEM-768 before the initiator's identity; bulk data in `X25519MLKEM768` TLS or sealed with the hybrid Noise keys | K10 (partly, §5) |
| S18 | Hidden responders reveal nothing before the right to connect is proven | A2/A3/former contacts probe for a hidden node | per-invitation and contacts masking keys never published; XK only; transcript-bound access proof; uniform silent close at a random 0.5–3 s for every failure; rendezvous IDs instead of node IDs at relays | K5 |
| S19 | The initiator reveals its identity only to the intended responder | anyone on the address learns who calls | identity in message 3, after message 2 proved the card's `hs` key | K5 |
| S20 | No downgrade of suites, features or services | A1/A4 strips options | protocol name, HS_HELLO and payload lists in the transcript; `offered` fixed by the signed card; responder enforces its best common suite | K3 |
| S21 | Puzzle before Noise work; admission by proven identities only | A4 floods handshakes or claims a known peer's key | cookie and solution checked before any public-key operation; identity known only after message 3; pass lookup by `pass_id`. The carrier TLS before the puzzle is bounded by per-source limits and Retry, not by the puzzle | K5, V4 |
| S22 | No collateral bans | A1 behind a CGNAT gets thousands of users banned | no pre-authentication IP bans; per-source pressure raises puzzle difficulty | — |
| S23 | 0-RTT data limited and replay-bounded | A1 replays message 1 | XK only, time window, replay cache keyed by the ephemeral key, allow-list of operations | V3 |
| S24 | Explained refusals; unknown input is never punished | v1: new frame types got peers banned | REJECT/UNSUPPORTED inside the channel; GREASE in features, suites, services and frames | K3 |

## 3. Objects, DHT, services

| # | Property | Adversary / attack defeated | Mechanism | Addresses |
|---|---|---|---|---|
| S25 | Node information cannot be forged or overwritten by third parties | A3/A4 injects SNI markers, server flags or addresses | only signed cards carry addresses, roles, SNI and masking keys; highest `seq` per scope wins; verbatim forwarding | K4 |
| S26 | No reflection through other people's cards | A3 signs cards pointing at a victim's address | `NODES` and PEX pass only cards connected to at their own address within 24 h; FORWARD dials only verified cards and public addresses; punch targets only peer-confirmed addresses | V4 |
| S27 | DHT lookups resist cheap hiding and rollback | A3 with a few VPS grinds IDs near a key and answers "not found" or old records | lookups ask all 20 closest of a diversity-limited set over 3 disjoint paths and take the highest `seq`; day-salted keys make pre-ground positions stale daily; readers never accept a lower `seq` than they have seen. **Limit:** an adversary that controls many networks (diversity units) near a key can still hide a record from a first-time reader (§4.9) | K4 (partly) |
| S28 | Routing table resists address rewriting and subnet flooding | A3 rewrites addresses or fills buckets from one subnet | entries only from sessions or signed cards; ≤ 2 per diversity unit per bucket; IPv6 counted per /32. **Limit:** as S27 | K4 (partly) |
| S29 | One encoding per object, yet open to extension | A4 re-encodes or pads a signed object; v2.0 rejecting v2.x objects | signatures over exact bytes; canonical rules checkable for unknown fields; unknown flags, enums and labels tolerated; equivocation resolved by one rule | K3, P4 |
| S30 | Release manifests cannot switch the network off | A compromised or coerced release key | 2-of-n signatures; key rotation and revocation by threshold; `seq` bounded by time; refusals never apply to a node's own version; no instant deprecation; no automatic installs | — |
| S31 | Sealed sender and padded messages | A4 (relay, mailbox) reads sender, receipt shapes, lengths | SealedEnvelope; plaintext padded to buckets; typed control fields | K7 (partly) |
| S32 | Two-hop mailbox delivery with receipts and spam limits | A4 mailbox learns both ends; floods a public queue; resets quotas with new sessions | forwarding relay ≠ mailbox relay; queue IDs instead of peer IDs; FETCH/ACK; one hop; per-day quotas per node ID and network; proof of work for public queues; duplicate suppression | K7, K9 |
| S33 | Relay circuits are end-to-end protected, cannot be hijacked, and are no presence oracle | A4 relay reads inner types; a neighbour claims to be the source; a prober asks relays whether a node is there | end-to-end XK inside circuits, sealed records with one counter per session; uniform delayed refusals; unlisted nodes reachable only by rendezvous IDs | K5, K7, K8 |
| S34 | Chat sessions are not broken by replays; no classic-only sessions | A4 replays an old message to force a reset; A3 replays a pre-upgrade bundle | silent drop; no resets on failure; KEM prekey mandatory, classic init refused | K8, K10, audit 7-8 P1 |
| S35 | Call media is authenticated per sender; no nonce reuse | A4 call member forges another's media or "mutes" a sender; a restarted sender reuses nonces | batched Ed25519 authenticators (one signature per ≤ 100 ms batch, every unit hashed, each batch covered twice) with the sender's per-call key; candidates held until verified; marking after verification; per-join keys; rekey on membership change; NACK budgets. **Limit:** forged media can be played for up to ~100–200 ms before detection (§4.17) | P1 |
| S36 | Call signals hidden from relays and signed over exact bytes | A4 relay reads the tree, VAD and NAT types | signalling only over end-to-end sessions; originator signature over bytes | P8 |
| S37 | File transfers bound to the authenticated peer | A4 group member cancels or redirects another's download | session-bound transfers; sender serves only named recipients | P7 |
| S38 | The exit does not learn the client's messenger identity | A4 exit links a VPN user to his chat identity | per-exit client identity; payment data only to served clients; random pay tags. **Limit:** the vault links a client's payments across exits (§4.8) | K6 (partly) |
| S39 | The exit is a poor scanner and reflector | A1 scans or reflects through exits | `unreachable` hides closed vs filtered (not open vs closed, not timing); DNS-rebinding check; TCP and UDP rate limits; credit caps; price checked against the signed offer | V4, V5 |
| S40 | Hole-punch addresses only to authenticated peers | A3 asks any node for its public, IPv6 and LAN addresses | coordination inside an end-to-end session; targets only relay-observed or card addresses; LAN only on matching public IP | K7, V4 |
| S41 | Bounded amplification | A4 turns small requests into large answers | per-type frame maxima, PEX/DHT rate limits, proxied-lookup buckets, no bandwidth-probe service, NACK budgets | V4 |

## 4. Residual risks

1. **Active confirmation with a masking key.** Whoever holds a masking key — every client of a public
   REALITY node, every contact of a hidden node, the holder of a leaked invitation until it is revoked —
   can build valid first packets and see that a node answers. REALITY public nodes are confirmable by
   design; noise3 keys are shared only with peers and invitees. A revoked contact keeps this ability
   until the contacts key rotates.
2. **REALITY intercept-and-replay.** An on-path attacker who blocks a client's real ClientHello and
   sends it himself within ±300 s is admitted and can compare the node's answer with the cover's.
   Mirroring makes them alike up to the end of the handshake, not afterwards (record timing, server
   behaviour under the real protocol).
3. **Bulk data post-quantum confidentiality** rests on `X25519MLKEM768` TLS or on sealed mode; a bug in
   the group check would weaken it silently.
4. **Fully random first datagrams.** A noise3 Init is a ≥ 1271-byte datagram without any structure.
   "Fully encrypted traffic" (FET) classifiers can flag the class without knowing any key. The GFW
   applies its FET rule to TCP today; moving the rule to UDP is a cheap evolution, so `noise3` is
   used for same-country hops only (D21) and is not claimed as protection against a global passive
   censor. Protocol-shaped first packets are future work (`wire` §2.7); the first public report of
   UDP FET is a monitoring trigger. Flow timing, sizes beyond the profile, connection counts and
   data-centre destinations remain visible; a censor that blocks all unknown UDP is not defeated by
   masking, and the volume freeze of TCP flows is a separate residual risk (§4.22).
5. **noise3 has no L0 forward secrecy** (Decision D11): a stolen `mask_sk` exposes recorded flows' inner
   QUIC packets (still TLS-protected) and makes them recognisable after the fact.
6. **Signatures are classical.** Ed25519 identities are forgeable by a future real-time quantum
   adversary; algorithm tags allow the move to hybrid signatures, v2 does not make it.
7. **Sealed sender and the ratchet are classical after the first message.** A future quantum
   adversary can unseal recorded envelopes (senders) and, after the PQXDH start, ratchet steps; SPQR
   is later. On relay paths `P1` (with `pass_id`) is protected only by `es` (X25519): a relay that later
   gains a quantum computer could link `pass_id`s across handshakes.
8. **Metadata at relays, exits and guards.** A relay sees both ends of a circuit (rendezvous IDs hide
   only unlisted targets from strangers), sizes and timing; a mailbox relay sees the forwarder; an exit
   sees all destinations and volumes; the TON vault links a client's payments across exits until a
   vault contract with per-exit keys exists; a guard running proxied DHT lookups learns whom its client
   looks up. EchoFog is not a mixnet.
9. **Sybil resistance is economic.** Node IDs are cheap to grind. The DHT defences raise the cost of
   hiding or rolling back a record to controlling a majority of a diversity-limited closest set over 3
   disjoint paths every day, but an adversary with addresses in many networks (a national ISP, a large
   cloud) can still hide a record from a reader who has never seen it.
10. **XX to a bare address authenticates nothing.** It is only a source of candidates and runs under a
    throwaway identity; an attacker on that path can feed false candidates (eclipse at first start)
    until signed bootstrap entries are used.
11. **0-RTT data** is not forward-secret against theft of the responder's handshake key and is
    replayable once per responder within 60 s; only public queries are allowed in it.
12. **Clock dependence.** L0 windows are ±300 s; a node learns its offset from authenticated peers and
    from the majority of several CDN `Date` headers (registries.md §12). Where a censor blocks or
    falsifies most of those endpoints, a node with a clock off by more than 5 minutes and no prior peer
    cannot dial any camouflaged or masked address until the user corrects the clock.
13. **Hidden-node invitations are bearer secrets**; whoever holds a link can enter until it is revoked.
14. **Group semantics** (P2–P6, P10) are application-level and unchanged by v2; chat/2 carries the group
    log as before. **Call traffic shape** (P9) is unchanged: voice and video rates and timers are
    recognisable inside camouflaged carriers.
15. **The handshake is cross-checked, not formally verified.** Two independent step-2 implementations
    and the reference generator agreed byte for byte on the first transcripts; the current normative
    file (`handshake-transcript.json`, application-valid inputs) still has to be reproduced by both; there is no machine-checked proof of the XKhfs/XXhfs composition with
    the prologue binding.
16. **Exit result timing** still separates "open" from "closed" for a client that measures it.
17. **Batched call authentication** lets an insider's forged media be played for up to about one batch
    plus one-way delay (~100–200 ms) before receivers detect it and stop playing unverified units from
    that sender; strict mode removes the window at up to 100 ms of added latency.
18. **The account key is a long-term secret on the device** in v2 (one device). Its theft (A6) lets the
    thief sign new device lists for the account; recovery is a new mnemonic, so a new node ID.
19. **Handshake-key rotation timing on success.** The responder tries its current `hs` key, then a full
    second pass with `prev_hs`; the extra pass is measurable. The uniform close hides it on failure,
    but a successful handshake shows whether the initiator used the current or the previous key. This
    reveals only which valid card generation an initiator holds; accepted. An operator that cares can
    shorten the overlap, at the cost of rejecting cards issued just before a rotation.
20. **TLS-in-TLS and cross-layer RTT inside exit traffic.** Nested TLS handshakes are detectable
    protocol-agnostically, and multiplexing and padding alone do not remove the class (USENIX
    Security 2024); correlating outer and inner round-trip times gives a second signal (NDSS 2025).
    v2.0 narrows this only partly: the client and exit/2 pad the first 8 records or 16 KB in each
    direction of each new exit stream into `shape` buckets in the same carrier record, add jitter
    ≤ 30 ms and mix streams on one carrier (T10.1/T10.2). Chat and signalling get no such
    protection. The residual is measured by the TLS-in-TLS and RTT detectors; the acceptance
    threshold is set at the M4 audit, and cover traffic on an exit path would be a separate decision,
    not current behaviour.
21. **Country allow-lists and hosting-range blocks.** A network can replace block-lists with
    allow-lists of SNI, addresses and services, and block hosting ranges by ASN or CIDR. Signed
    cards, cover selection and REALITY do not make a node reachable where its address or cover SNI is
    not allowed. The answer is operational, not cryptographic: a first hop in the user's own country
    (T6.4), covers filtered by the signed allowed-SNI/CIDR file (T2.6c), and a "domestic entry"
    preset and document (T12.5) whose address lies in a CIDR allocated to a service on the
    allow-list, not in an arbitrary national cloud — operators have already separated cloud ranges,
    so a neighbouring VPS is not a workaround (reports, January 2026). Where such an entry cannot be
    obtained, Russian mobile networks behind allow-lists are not supported in v2.0.
22. **Volume freeze on Russian mobile networks.** A TCP flow to a foreign data centre can be frozen at
    about 16–20 KB (25 packets) without a reset; the state is keyed to the source address and
    destination SNI, with a penalty of about 120 s and up to 600 s after a change of fingerprint
    (reported in 2026). The answer is the same operational one as §4.21: from Russian mobile
    networks no direct TCP to a foreign data centre above 12 KB, no blind UDP to foreign addresses,
    and the first hop is domestic (T6.4); `stream-resume` is not raised for v2.0. A session can move
    to another path, but a frozen stream does not resume on the same carrier.
23. **Application-level VPN detection.** Since 15 April 2026 applications in Russia are required to
    detect VPN use, and 22 of 30 popular applications do. This is outside the protocol: carrier
    shape and cover selection cannot hide the tunnel from an application running on the same device.
    Product measures only: exclude selected applications from the VPN on Android (T12.2), warn in
    the UI, and say in the release notes that invisibility from applications is not promised. A VPN
    interface such as `tun0` stays visible to any application through the interface list.
24. **Slot takeover on an unauthenticated Init (R-1, accepted).** An attacker who can forge the UDP source
    address, knows the responder's `mask_pub` and the victim's IP:port, and sends an Init followed by data
    packets keyed to the Init sender's keys can take the slot of a noise3 flow (wire §2.5, §2.6). The effect
    is that one handshake fails; the client retries and gets through. Nothing is decrypted or altered. Accepted
    as a residual risk (decision of 2026-11-05). Binding the slot to the address changes the wire behaviour,
    so it is deferred to v2.1 and is to be reviewed together with an audit of the carriers.
25. **Empty device unit on relayed sessions (F3, accepted).** On a relayed session the device unit of `Peer`
    is empty, so the per-unit limit does not apply to that peer. The limits there are enforced by the session
    itself and by the signer of the peer's card. The per-unit guarantee holds on direct paths only. Accepted
    as a residual risk ("direct path only"); to be revisited with the relay limits of T6.1.

## 5. Coverage of the v1 weaknesses

| ID | Status | Notes |
|---|---|---|
| K1 | **fixed** | masking keys random and not in the ID; noise3 keys unpublished and selector keyed on ECDH; residual: active confirmation by key holders (§4.1) |
| K2 | **partly** | carrier rules, no plaintext STUN, unobservable noise3 keys; residual: fully random first datagram class (§4.4); WebRTC-shaped carrier later |
| K3 | **fixed** | per-layer versions, GREASE, extension-safe objects; L1 cross-checked by two independent implementations (§4.15) |
| K4 | **partly** | signed cards, UKS binding, reflection rules fixed; DHT hiding by a many-network adversary remains (§4.9) |
| K5 | **fixed** | XK-only hidden nodes, transcript-bound proofs, uniform failure, rendezvous IDs, personal passes; residual §4.1, §4.2 |
| K6 | **partly** | per-exit client identity, no neighbours or public pay data, DoH; vault linkability and destination visibility remain (§4.8) |
| K7 | **partly** | sealed sender, padding, two-hop mailbox, private punch addresses; ratchet header encryption later, `to` visible, relays see both ends |
| K8 | **fixed** | end-to-end circuits, silent replay drops, refusals naming the target |
| K9 | **fixed** | one hop, receipts, per-day quotas, PoW for public queues; spam economics remain a tuning question |
| K10 | **partly** | hybrid handshake, PQXDH mandatory, PQ bulk data; ratchet, sealed sender and signatures classical (§4.6, §4.7) |
| V1 | **partly** | full cover forwarding, mirroring, address-bound tokens, persisted caches; intercept-and-replay remains (§4.2) |
| V2 | **partly** | profiles, jitter, padding everywhere; first-datagram class, call shape and resumable tunnels remain |
| V3 | **fixed** | 0-RTT, merged messages, no per-message envelopes |
| V4 | **fixed** | amplification limits, reflection rules, rate limits (all local policy) |
| V5 | **fixed within the current contract** | credit caps, price check, code allow-list; per-exit payment keys need contract v2 |
| V6 | **fixed** | one frame format, one padding mechanism, one card, one time unit |
| V7 | **fixed** | dead mechanisms not specified |
| N1 | **fixed** | 128-bit selectors and AEAD; signed observations |
| C12 | **fixed** | roles from signed cards, saved with the routing table |
| P1 | **fixed** | per-sender signatures, per-join keys, rekey on membership change, verification before marking |
| P2–P6, P10 | **not fixed (out of scope)** | group membership and log sync are application-level; separate groups work |
| P7 | **fixed** | session-bound transfers |
| P8 | **fixed** | signals over end-to-end sessions, signatures over bytes |
| P9 | **not fixed** | call shaping profile reserved (`constant-rate-call`) |
| P11 | **partly** | dead application code is not specified; group join round trips are application-level and unchanged |
