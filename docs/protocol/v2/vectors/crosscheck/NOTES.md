# Clean-room handshake: notes

A second, independent implementation of the L1 handshake (handshake.md, Noise XKhfs and XXhfs),
written only from `handshake.md`, `registries.md`, `versioning.md`, `README.md` §5–§6, `objects.md`
(the `akey` format) and the existing vectors. The code under `internal/v2/handshake` and
`internal/v2/noise` was not read, nor the generator's own Noise code (`vectors/gen/noise.go`).
Its only purpose is Decision D15: two implementations must produce byte-identical transcripts
before L1 is final.

```
GOWORK=off go run . -out .     # writes transcript-cleanroom.json
GOWORK=off go test .           # vectors, normative transcripts, round trips, tampering
```

## Structure (deliberately unlike a state machine)

- No `SymmetricState`/`CipherState`/`HandshakeState` types. Each role of each pattern is one
  straight-line function (`xkInitiator`, `xkResponder`, `xxInitiator`, `xxResponder` in
  `handshake.go`) that keeps `h`, `ck`, `k`, `n` as local variables and calls pure helpers
  (`mixHash`, `mixKey`, `encHash`, `decHash`). The token order is written out line by line from the
  tables of §4.1 and §4.2; there is no pattern interpreter.
- The two roles run in two goroutines over channels; both log `h`, `ck`, `k`, `n` after every token,
  and `run` requires the two logs, the messages, the payloads and the final keys to be identical.
- Payload protobufs are produced by a separate minimal writer (`payloads.go`) from the field
  descriptions, and checked against the `encoded` values of `handshake.json`.

## Status after the spec update (merge of `spec-v2`)

The runs now use the normative inputs: `handshake.json` `transcript_inputs` (the `hs/xk/*` seeds for
all three patterns), `prologue_xk`, `prologue_xk_hidden`, `prologue_xx_bare_address`, the payloads
`HsInit_xk`, `HsInit_xk_hidden`, `HsInit_xx`, `HsResp`, `HsResp_xx`, `HsFinish`, `HsFinish_xx`, and
the `initiator` / `initiator_throwaway` cards of `node-card.json`. The hidden `HS_HELLO` is built with
the §3.4 GREASE rule, the P2 card is chosen by the §7 "Card in P2 (XK)" rule, and `ck` is erased only
after `Split`, `session_secret` and `sid` (§9, `finish` in `handshake.go`).

`transcript-cleanroom.json` was regenerated and committed (5b67d50) **before**
`handshake-transcript.json` was opened. Comparison afterwards (`TestNormativeTranscript`):

| transcript | prologue | P1/P2/P3 | steps h, ck, k, n | h before each payload | message bytes | final h, ck, k_i2r, k_r2i, session_secret, sid | transport_example |
|---|---|---|---|---|---|---|---|
| `XKhfs` | equal | equal | 14/14 equal | equal | 3/3 equal | equal | equal |
| `XKhfs-access-proof` | equal | equal | 14/14 equal | equal | 3/3 equal | equal | equal |
| `XXhfs` | equal | equal | 14/14 equal | equal | 3/3 equal | equal | equal |

**No value differs.** Only names differ: the step labels (`init`, `prologue`, `pre:s` there;
`InitializeSymmetric`, `MixHash(prologue)`, `MixHash(rs) pre-message` here), the JSON field names
(`bytes` there, `noise_message` here; for "no key", `k` is `""` there and omitted here), and the
transcript ids, which were renamed to the normative names. The clean-room file keeps its own layout,
so the generator's `-crosscheck` option cannot read it as is; `TestNormativeTranscript` does the
comparison instead.

## Tests

All 15 pass:

- `handshake.json`: `initialize_symmetric`; the three prologues and `h_after_prologue`;
  `h_after_premessage_rs`; `rid` = the device ID; `access_example`; every `payloads.*.encoded`
  (`HsInit_xk_hidden` after a run, because its proof is over the real `h1`); `puzzle`; every
  `transcript_inputs` value; the §3.4 GREASE examples.
- `kdf.json` `session`: Split keys, `session_secret`, `sid`, `sealed_record`.
- `handshake-transcript.json`: everything in the table above.
- Round trips of all three runs, with these checks:
  - message overheads are 1232/1152/64 bytes for XK and 1216/1200/64 bytes for XX;
  - the §7 card checks pass: `hs_key` = `0x02 ‖ rs`, the signer's device ID equals the
    `device_id` of `node-card.json` `initiator` / `initiator_throwaway`, and the `hs_key` of the
    XX P2 public card matches `rs`. Signatures and device lists are not verified.
- Failure cases: tampering with any message, a wrong prologue, zero and small-order DH, a
  non-reduced ML-KEM key.

## Earlier ambiguities A1–A19: how the spec resolved them

The quoted originals are in the history of this file (commit ee165c3).

| | Resolution in the merged spec | Here |
|---|---|---|
| A1 XX inputs | `transcript_inputs` = XK seeds for all three transcripts | followed; `hs/xx/*` dropped |
| A2 XX payloads | §4.2 "MUST contain only ..."; `HsInit_xx`, `HsResp_xx`, `HsFinish_xx` | followed |
| A3 XX card scope | §4.2: the public card | followed |
| A4 P2 card in XK | §7 "Card in P2 (XK)"; `known_card_seq` = the public card's `seq` | followed (`p2Card`) |
| A5 initiator card | `node-card.json` `initiator`, `initiator_throwaway` | followed; card checks pass |
| A6 32 vs 33 bytes | §2 "Key bytes" | as already read |
| A7 h1 of the proof | §5 "of this handshake"; `XKhfs-access-proof` | followed |
| A8 GREASE in `offered` | §3.4: before the first larger entry | followed (`insertGrease`) |
| A9 `known_card_seq` in XX | §4.2: malformed | absent |
| A10 `n` without a key | §2, explicit | as already read |
| A11 `hs` rotation | §4.1: current key first, then a full second pass | not implemented (one key) |
| A12 `rid` | §3.3 "always a device ID" | as already read |
| A13 hidden cards list XX | §3.1: never 0x0002; the invite card now has `l1_suites [1]` | consistent |
| A14 what is compared | §12 D15: Noise message bytes, steps, finals | compared exactly that |
| A15 `pad` | absent in vectors | absent |
| A16 section order | §3.4 now after §3.3 | resolved |
| A17 byte order | §6 `u64be` | as already read |
| A18 erase order | §9 "Order" | followed |
| A19 hidden-node close | §5 / §11: the uniform close wins | not modelled (no timing) |

## Still open or new after the update

### B1. A `known_card_seq` that names no current card

§7: "The responder includes its current card of that card's scope in `P2` exactly when that card has
a higher `seq`". This assumes the responder can map any `known_card_seq` to a scope. If the value is
the `seq` of an old card the responder no longer remembers, or `0`/absent (an XK initiator that got
the `hs` key some other way), the scope is unknown and the rule gives no answer. **Chosen here** (the
vectors do not exercise it): send the current public card. The spec should say what to send, and
whether a responder must keep a `seq` → scope map for every card it has issued.

### B2. `pad` in XX P1

§4.2: "`P1` in XX MUST contain only `proto_ver`, `features`, `services` and `time`". §7: "`pad`, when
present, is the last field". Read literally, the first sentence forbids `pad` in XX P1 and the second
allows it everywhere. A responder that enforces "only" closes on a padded XX P1. The vectors have no
`pad`. Suggest "only ... `time`, and optionally `pad`".

### B3. GREASE in `offered`: how many, and what the responder checks

§3.4 says "one GREASE value". §3.1 lists the malformed hellos: repeats, `chosen` not in `offered`,
GREASE as `chosen`, a bad count, trailing bytes. It does not say whether two different GREASE values,
or an `offered` made only of GREASE, is malformed. It also does not say whether the responder checks
that `chosen` is the *first non-GREASE* entry, or only that it is its own most-preferred suite in the
intersection. **Chosen here:** the builder emits at most one GREASE value. This cross-check has no
responder-side `HS_HELLO` parser.

### B4. The throwaway identity reuses the initiator's `hs` key

§4.2 defines a throwaway identity as "a fresh Ed25519 key and a minimal card, never reused"; it does
not say that the X25519 `hs` key must be fresh too. In the vectors, `initiator_throwaway` has the
same `hs_key` as `initiator` (`x25519(hs/xk/initiator_s)`), and the XX transcript sends that key as
`s`. This causes two problems:

- (a) the `hs` key links the throwaway session to the real identity, which defeats its purpose;
- (b) §7 "hs_key ↔ device ID binding" says "A card whose `hs_key` ... is already bound to a
  different signer is rejected and counted as abuse". A responder that has seen the real `initiator`
  card rejects the throwaway card and counts it as abuse.

The transcript bytes are fine, because shared keys are a deliberate vector choice. But the spec should
require a fresh `hs` key for a throwaway identity, and the vector note should say that the shared key
is a test shortcut a real node would refuse.

### B5. The AD of `transport_example` is not the wire.md §8.4 record AD

`final.transport_example` uses `ad = sid`, while wire.md §8.4 seals records with
`aad = sid(16) ‖ n(8)` (reproduced from `kdf.json` `sealed_record`). The vector's note says it checks
only the keys and the nonce format, so this is not a contradiction. Still, a reader who copies it as
a record format gets it wrong; a pointer to §8.4 would help.

### B6. What D15 compares vs. what `-crosscheck` compares

§12 D15: implementations must reproduce "every step's `h`, `ck`, `k`, `n`". vectors/README.md says
that `-crosscheck` "compares the message bytes and final hash". So a file can pass `-crosscheck` and
still differ in an intermediate `ck` or `k`. This is a gap in the tooling, not in the spec;
`TestNormativeTranscript` compares every step.

### B7. Key rotation (A11) has no vector

The resolved text is clear: "processes message 1 with the current key first; if `P1` does not
decrypt, it repeats the initialisation and message 1 with the previous key". But no transcript runs
under `prev_hs`. On a hidden node, the second pass happens before the access check, because the check
needs the decrypted P1. A vector of message 1 under `prev_hs_sk`, with the responder's expected `h1`,
would pin both.
