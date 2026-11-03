# Clean-room handshake: notes

A second, independent implementation of the L1 handshake (handshake.md, Noise XKhfs and XXhfs),
written only from `handshake.md`, `registries.md`, `versioning.md`, `README.md` §5–§6, `objects.md`
(the `akey` format) and the existing vectors. The code under `internal/v2/handshake` and
`internal/v2/noise` was not read. Its only purpose is Decision D15: two implementations must
produce byte-identical transcripts before L1 is final.

```
GOWORK=off go run . -out .     # writes transcript-cleanroom.json
GOWORK=off go test .           # vectors of handshake.json / kdf.json, round trips, tampering
```

## Structure (deliberately unlike a state machine)

- No `SymmetricState`/`CipherState`/`HandshakeState` types. Each role of each pattern is one
  straight-line function (`xkInitiator`, `xkResponder`, `xxInitiator`, `xxResponder` in
  `handshake.go`) that keeps `h`, `ck`, `k`, `n` as local variables and calls pure helpers
  (`mixHash`, `mixKey`, `encHash`, `decHash`). The token order is written out line by line from the
  tables of §4.1 and §4.2; there is no pattern interpreter.
- The two roles run in two goroutines over channels; both log `h` and `ck` after every token, and
  `run` requires the two logs, the messages, the payloads and the final keys to be identical.
- Payload protobufs are produced by a separate minimal writer (`payloads.go`), and checked against
  `handshake.json` encodings.

## What reproduces the existing vectors (tests)

All of these pass:

- `handshake.json`: `initialize_symmetric` (h0 = ck0 for both names), both prologues and
  `h_after_prologue`, `h_after_premessage_rs`, `rid` = `node-card.json` device ID, `access`
  (`pass_id`, `proof` over the seed `hs/h1`), `payloads.HsInit` / `HsResp` encodings, `HsFinish`
  SHA-256 and length, the whole `puzzle` block (cookie, nonce 2274, hash, `HS_RETRY` frame, hello
  with solution), and every value of `deferred_transcripts.inputs_xk` (public keys, ML-KEM `ek`
  hash, ciphertext hash, shared secret).
- `kdf.json` `session`: Split keys, `session_secret`, `sid`, and the `sealed_record` with `k_i2r`.
- In the transcripts: the message overheads are exactly §4.1's 1232 / 1152 / 64 bytes for XK, and
  the h after the XK pre-message and the XX prologue equal `handshake.json`.

There is no existing full transcript to compare against (D15), so the transcript bytes themselves
are checked only by the internal round trip and must be compared with the main implementation.

## Transcripts written

`transcript-cleanroom.json` has four runs. For each: seeds used, public keys, `HS_HELLO` payload and
frame, `cb`, `rid`, prologue, P1/P2/P3 (hex, length, SHA-256), every step's `h`/`ck`/`k`/`n`, each
Noise message and its `HS_MSG` frame, the first flight (`HS_HELLO` frame ‖ `HS_MSG` frame 1),
`h1`, the ML-KEM hashes, card checks, and final `h`, `ck`, `k_i2r`, `k_r2i`, `session_secret`, `sid`.

| id | what | why |
|---|---|---|
| `xk` | XK with `deferred_transcripts.inputs_xk` and P1/P2/P3 **literally** `payloads.*` | the byte-for-byte target named by `handshake.json` |
| `xk_live_access` | same, but `P1.access.proof` over the run's real `h1`; responder verifies it | the literal P1 is not a valid hidden-node message (A7) |
| `xx` | XX with seeds `hs/xx/*`, bare-address prologue, XX-legal payloads | no XX inputs exist (A9) |
| `xx_with_xk_seeds` | XX reusing the `hs/xk/*` seeds | in case the other implementation reused them |

## Ambiguities, gaps and contradictions

Each item quotes the text and says which reading this implementation took. Items marked
**[transcript]** change bytes of the transcript if read the other way.

### A1. No XX transcript inputs [transcript]

`handshake.json` `deferred_transcripts` has only `inputs_xk`; handshake.md §12 D15 says "Full
message-by-message transcripts of XKhfs and XXhfs (fixed ephemerals, ML-KEM seeds and encapsulation
randomness) are to be produced", and README.md "`handshake.json` fixes the inputs to use". For XX it
does not. **Chosen:** seeds `hs/xx/initiator_e`, `hs/xx/initiator_s`, `hs/xx/responder_e`,
`hs/xx/initiator_e1_d`, `hs/xx/initiator_e1_z`, `hs/xx/encaps_m` (same suffixes as XK), responder
static = `node-card.json` `hs_sk`, prologue = `prologue_xx_bare_address`. A second run reuses the
`hs/xk/*` seeds. The spec should name the XX inputs, including which prologue (bare address, or
`flags = 0x02` with an expected device ID, which has no vector).

### A2. Which payloads in XX [transcript]

`deferred_transcripts` says "P1 = payloads.HsInit, P2 = payloads.HsResp, P3 = payloads.HsFinish"
only for XK. These cannot be used in XX: §4.2 "P1 in XX MUST NOT contain `access` or `early`, and
SHOULD contain only `proto_ver`, `features`, `services` and `time`", while `payloads.HsInit` carries
`access` and `known_card_seq`; §7 "`card` ... REQUIRED in XX", while `payloads.HsResp` has no card.
**Chosen:** XX P1 = `payloads.HsInit` without fields 4 and 5; XX P2 = `payloads.HsResp` plus
field 4 = the `node-card.json` **public** card envelope (inserted in field order, before
`observed`); XX P3 = `payloads.HsFinish` unchanged.

### A3. Which card scope the responder sends in XX P2 [transcript]

§7 says only "responder card: REQUIRED in XX". The responder has public, peer and invitation cards
(objects.md §10.3); the peer card carries noise3/ws2 addresses and `prev_hs`, which a bootstrap
stranger should not learn. **Chosen:** the public card. The spec should say which scope.

### A4. `payloads.HsResp` has no card although the rule requires one [transcript]

§7: "`card` ... in XK if newer than `known_card_seq`". `payloads.HsInit` sets
`known_card_seq = ts − 86400 = 1793577600`, and every card of `node-card.json` has a larger `seq`
(1793664000..2). So a conforming responder would include its card in P2. **Chosen:** literal
`payloads.HsResp` (no card) for the `xk` runs, as the vector file says. Either fix
`known_card_seq` to the card's `seq` or add the card.

### A5. `payloads.HsFinish` carries the responder's card [transcript-validity]

`HsFinish.fields` = "card = node-card.json peer card (format illustration)". In a transcript this
card is the *initiator's* card, so §7 "Card checks ... `card.hs_key` equals the Noise remote static
key (`rs`)" fails: its `hs_key` is the responder's, not `hs/xk/initiator_s`. A conforming responder
rejects after message 3 (the transcript bytes are unaffected; `card_checks.p3_card_hs_key_matches_rs`
is `false` in every run). There is no initiator card in the vectors. **Chosen:** use it literally,
record the failed check. The vectors need an initiator card whose `hs_key` is `x25519(initiator_s)`.

### A6. `rs` is 32 bytes, `card.hs_key` is 33 [interpretation]

§4.1 `MixHash(rs)` "(the responder's `hs` public key, from the initiator's copy of the card)";
objects.md: `hs_key` is an `akey` = `0x02 ‖ pub(32)`. Which bytes are hashed is not said.
**Chosen:** the raw 32 bytes (this is what `h_after_premessage_rs` requires, so the vector settles
it), and the card check compares `hs_key` with `0x02 ‖ rs`. The text should say "the 32-byte X25519
key inside `hs_key`" for the pre-message, `s` token and the card check.

### A7. Access proof in the literal P1 is over the fixed seed `hs/h1` [transcript]

§5 "`h1` is the handshake hash immediately before `EncryptAndHash(P1)` (after step 1.3)", but
`payloads.HsInit.access.proof` is computed from the seed `hs/h1`, and `deferred_transcripts` reuses
that P1 verbatim. The resulting message 1 would be rejected by a real hidden responder. **Chosen:**
both: `xk` uses the literal P1; `xk_live_access` recomputes the proof over the real `h1` (which is
`derived."h1 ..."`). The spec should say which one D15 compares.

### A8. Preference order vs. sorted with GREASE in `offered`

§3.1: "`offered` u16 × n — suite IDs, in the initiator's preference order". §3.4: "an initiator
SHOULD add one GREASE value ... to `offered` ... Lists stay sorted with the GREASE value in its
numeric place". A preference-ordered list is not sorted in general (e.g. preferring 0x0002 over
0x0001), so "sorted" either applies only to `features`/`services` or forces numeric order on
`offered`. **Chosen:** no GREASE in `offered` (the vector hellos have none; it is only a SHOULD),
so it does not matter for these bytes. The reading here is that "sorted" is meant for
`HsInit.features`/`services` (§7), which leaves the position of GREASE in `offered` unspecified.
Two implementations that do add GREASE may disagree on its position, and the prologue hashes it.

### A9. `known_card_seq` in XX

§7 marks it "(XK)" but does not forbid it in XX the way `access`/`early` are; §4.2 "SHOULD contain
only ..." excludes it softly. **Chosen:** absent in XX.

### A10. `EncryptAndHash` without a key

§2.1: "appends `EncryptAndHash(e1.public)` (1184 bytes, plus 16 if a key is set)". In both patterns
`e1` (and XX's P1) come before any `MixKey`, so they are plaintext. Noise §5.2 is followed literally:
with an empty key, EncryptAndHash is `MixHash(plaintext)` and `n` does not move. Not ambiguous, and
an implementation that wrongly advanced `n` would still agree, because the next `MixKey` resets `n`;
`steps[].n` records it anyway.

### A11. Key rotation: what the responder retries [behaviour]

§4.1: "During an `hs` rotation the responder holds a current and a previous key (§7.1) and tries
both: one extra X25519 per message 1". Because `MixHash(rs)` is the pre-message, a different key
changes `h` from the start: the retry is MixHash(prologue), MixHash(rs'), MixHash(re), MixHash of
the 1184-byte `e1`, one X25519 and one AEAD open, not just one X25519. Not said: the order of tries
(current first?), and that only the P1 AEAD distinguishes the keys (with a P1 decryption failure
for both being the uniform failure). Not implemented here (one responder key only).

### A12. `rid` naming: device ID vs node ID

§3.3 says `rid` is the "expected responder device ID (the signer of its card)", §4.2 says "checks
`SHA-256(card signer akey) = rid`", but `prologue_xx_bare_address.note` says "no expected node
ID", and §7 card checks speak of "device ID or node ID". The vector's `rid` is the device ID
(SHA-256 of `0x01 ‖ device pub`), reproduced here. **Chosen:** device ID; the note should say
device ID.

### A13. A hidden node's cards list the XX suite

D22: "A hidden node MUST NOT accept XX". D4: "`offered` MUST contain exactly the suites that are both
supported by the initiator and listed in `l1_suites` of the ... responder's card". The invitation
card in `node-card.json` has `l1_suites = [1, 2]`, and the XK hello offers `[0x0001, 0x0002]` to a
responder that the P1 `access` field shows to be hidden. Harmless for the bytes, but then a hidden
node's card advertises a suite it refuses; either cards of hidden nodes should omit 0x0002 or the
offered rule should allow it.

### A14. What "the exact bytes of each message" are

§4: "Each Noise message is sent as one `HS_MSG` frame whose payload is the message bytes", and the
first flight is `HS_HELLO` "immediately followed by" `HS_MSG`. The transcripts give the Noise
message, its `HS_MSG` frame (type 0x03, flags 0, QUIC varint length) and the first flight; the spec
should say which of them D15 compares (I recommend the Noise message bytes plus the frames).

### A15. `pad` and determinism

§7 "`bytes pad = 15; // random length, zero bytes`" makes payloads non-deterministic; the vectors use
no `pad`. **Chosen:** no `pad` (field absent, not an empty field). Should be stated as an input of
D15 ("pad absent").

### A16. Order of the sections

§3.4 (GREASE) appears between §3.1 and §3.2, and §3.3 (prologue) after §3.2, so the reader meets
`flags & 0x02` and `rid` before they are defined. Editorial only.

### A17. Byte order of `u64` in cookie and puzzle

§6 writes `u64(minute)` and `u64(nonce)` without byte order; README.md §6 "unsigned big-endian
unless a field says `le`" settles it, and the vector confirms big-endian. The AEAD nonce is the only
little-endian value (§2, explicit). Not ambiguous once README.md is read, but §6 could say "u64be".

### A18. `session_secret` vs erasing `ck`

§9 derives `session_secret` from `ck` and then says "All ephemeral secrets, `ck` and the KEM key pair
are erased". Implied order: Split and `session_secret` first, then erase. Fine; noted because `sid`
also needs the final `h`, which is not on the erase list (it is public to both sides).

### A19. All-zero DH and the ML-KEM check: where they abort

§2 "an all-zero output MUST abort", §11 "Any decryption failure, malformed token, all-zero DH
output, or invalid ML-KEM key ends the handshake by closing the carrier connection without a
message". For a hidden node §5 requires the uniform delayed close instead, and step 3 ("Message 1
tokens") covers the `es` DH; §11 reads as an immediate close. **Chosen:** both abort (the test
checks small-order and zero points and a non-reduced `ek`); timing is out of scope here. The text
should say that for hidden nodes §5's uniform close overrides §11.

### Things that were *not* ambiguous

Protocol name as the Noise `protocol_name` (D3, long name hashed), prologue layout, token order of
both patterns, HKDF/Split/session-secret/sid, `e1`/`ekem1` processing, ML-KEM seed `d ‖ z` and
encapsulation randomness `m`, the AEAD nonce and AD (= `h`), message sizes of §4.1. The existing
vectors pin all of these.
