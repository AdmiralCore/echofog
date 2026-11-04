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

## First update (merge of `spec-v2`)

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

All 18 pass (the list below was written for the first update; the second update adds `TestHelloChecks`, `TestXXP1Check`, `TestRotation`, and the rotation and sealed-record checks in `TestNormativeTranscript`):

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

## B1–B7 (first update), as raised

The quoted originals of B1–B7 are in the history of this file (commit 3451a16); the resolutions are
in the table below.

## Second update: B1–B7 settled (merge of `spec-v2`, e1d9a03)

New inputs and checks:

- **`XXhfs`**: the initiator static is the throwaway identity's own `hs` key, seed
  `initiator/throwaway_hs_sk` (= `node-card.json` `initiator_throwaway.hs_sk`). `HsFinish_xx`
  carries the new throwaway card, and the card check (`hs_key` = `0x02 ‖ s`) passes.
- **`XKhfs-rotation-prev-hs`**: the initiator's `rs` is the previous key (`card/prev_hs_sk`); other
  inputs are as in `XKhfs-access-proof`. The responder (`xkResponder` with `xkMessage1Pass`) first
  runs a whole pass over message 1 with its current key. When P1 does not decrypt, it runs a second
  whole pass with `prev_hs`, and only then the access check. The failed pass is written to
  `rotation.failed_pass`. If no previous key is held, the handshake fails (`TestRotation`).
- **B3 responder checks** (`hello.go`, `checkHello`, run by both responders before message 1).
  These are rejected as malformed:
  - a repeated value in `offered`;
  - more than one GREASE value, or an `offered` made only of GREASE;
  - a `chosen` not in `offered`, a GREASE `chosen`, or a `chosen` that is not the first
    non-GREASE entry;
  - a count outside 1..8;
  - trailing or missing bytes (the puzzle solution is accounted for).

  After these come the suite rule (`chosen` = own most preferred suite of the intersection;
  a hidden node supports only `0x0001`). `TestHelloChecks` covers each case.
- **XX P1 check** (`checkXXP1`): only fields 1, 2, 3, 6 and 15 (`pad` allowed, per B2's resolution).
- **§8.4 sealed record:** `sealRecord`, with `aad = sid ‖ u64(n) ‖ u8(class) ‖ varint(path_id) ‖
  varint(pipe)`; it reproduces the new `kdf.json` `sealed_record` (aad, sealed, record).

The regenerated `transcript-cleanroom.json` was committed (e1d9a03) before the comparison.

### Comparison with `handshake-transcript.json`

| transcript | prologue, P1–P3 | steps h/ck/k/n | h before payloads | message bytes | finals | sealed record example |
|---|---|---|---|---|---|---|
| `XKhfs` | equal | 14/14 | equal | 3/3 | equal | equal (see below) |
| `XKhfs-access-proof` | equal | 14/14 | equal | 3/3 | equal | equal |
| `XXhfs` | equal | 14/14 | equal | 3/3 | equal | equal |
| `XKhfs-rotation-prev-hs` | equal | 14/14 | equal | 3/3 | equal | equal |

The `rotation` block also matches. In the first pass, `rs`, `h_after_premessage`, `h1`,
`ck_after_es` and `k_after_es` equal our failed pass after `es`. In the second pass, `rs` and `h1`
equal ours.

**No value differs.** One input differs by choice only: before seeing the normative file I sealed
the `kdf.json` plaintext (`…hello over a relay`), while the normative `sealed_record_example` seals
`…hello over a sealed path` (MUX_STREAM on stream 4). Same `n` = 0, class 1, path 0, pipe 0.
Recomputed with the normative plaintext, the AAD, sealed bytes and record match for all four
transcripts (`TestNormativeTranscript`). Our JSON keeps our own example, labelled as a choice.

### B1–B7: how they were settled

| | Resolution | Here |
|---|---|---|
| B1 unknown `known_card_seq` | §7: remember 30 days of cards; else public card, or on a hidden node peer card (personal pass) / nothing (invitation pass) | public-node fallback implemented; the hidden-node fallback is not (no vector uses it) |
| B2 `pad` in XX P1 | §4.2 "and optionally `pad`" | `checkXXP1` allows field 15 |
| B3 HS_HELLO checks | §3.1 full list, then the suite rule | implemented, tested |
| B4 throwaway `hs` | §4.2 fresh `hs` key; new vector key | followed |
| B5 record AAD | wire.md §8.4 new AAD; `sealed_record_example` | followed |
| B6 `-crosscheck` scope | now every step | our own test compares every step as well |
| B7 rotation vector | `XKhfs-rotation-prev-hs` + `rotation` | reproduced |

## Still open

### C1. The rotation transcript's `known_card_seq` names a card that has the current key

`XKhfs-rotation-prev-hs` reuses `HsInit_xk_hidden`, so `known_card_seq` = `ts + 2`, the invitation
card. That card's `hs_key` is the **current** key and it has no `prev_hs`. Yet the initiator uses
`prev_hs` as `rs`. §7.1 says "Initiators always use `hs_key`". An initiator that really holds the
`ts + 2` card would therefore use the current key. A real rotation would mean the initiator holds an
older invitation card (a lower `seq`, whose `hs_key` was the old key). This changes no bytes,
because the vector fixes P1 explicitly. But this transcript is not as application-consistent as the
others: `known_card_seq` should name the old card. It also leaves open what the §7 P2 card rule then
sends, because that old invitation card is a seq the responder must remember for 30 days.

### C2. `flags & 0x02` is not in the list of malformed hellos

§3.3: "The initiator MUST set the flag and `rid` in XK". The §3.1 malformation list does not include
"XK `chosen` without flag `0x02`", so the spec does not say whether a responder must reject such a
hello or just build its prologue with `rid` = zero (the handshake would then fail anyway, by
transcript mismatch, if the initiator in fact bound a `rid`). `checkHello` here does not enforce it.
Suggest: add it to the list, or state that the responder follows the flag as received.

### C3. Rotation and the uniform delay

§4.1: the second pass is "a full second pass", and on a hidden node both passes come before the
access check. The extra pass costs measurable time only while a previous key is live; §5's uniform
close (0.5–3 s from message 1) hides it on failure, but on **success** the time to message 2 shows
whether the current key or the previous key was used. That tells an observer which card generation
the initiator holds. It is probably harmless, but it is not mentioned.
