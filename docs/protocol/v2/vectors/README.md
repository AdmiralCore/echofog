# Test vectors

JSON test vectors for the deterministic constructions of protocol v2, written by the reference
generator in [gen/](gen/). Byte strings are lowercase hex. Times use the fixed clock
`1793664000` (2026-11-03T00:00:00Z).

## Reproducing

```
cd gen
GOWORK=off go run . -out ..
```

The generator uses only the Go standard library (Go 1.27: `crypto/ecdh`, `crypto/ed25519`,
`crypto/hkdf`, `crypto/mlkem`, `crypto/mlkem/mlkemtest`, `compress/flate`), `golang.org/x/crypto`
(ChaCha20-Poly1305, BLAKE2s, X25519) and `filippo.io/edwards25519` (field and point arithmetic for
Elligator2). Every input is `SHA-256("echofog v2 test vector seed: " ‖ name)`; each file lists the
seed names and values it used under `seeds`. Running it twice gives identical files, except the
DEFLATE bytes in `link.json`, which depend on the compressor (decoding is what is normative).

Before writing, the generator checks itself against external references where they exist: the RFC
8032 TEST 1 key and signature, the RFC 9000 Appendix A.1 varint examples, the RFC 9380 Appendix J.4.2
Elligator2 map, the order of the torsion point `T`, `forward(reverse(u)) = u` for every Elligator2
key, equality of X25519 results for dirty and clean keys, ML-KEM decapsulation of its own
encapsulation, and signature verification and canonical field order of every signed object.

## Files

| File | Contents | Spec |
|---|---|---|
| `varint.json` | QUIC varints, valid and non-minimal (to be rejected) | wire.md §6 |
| `frames.json` | every session-level frame type, mux frames, a service frame, datagrams | wire.md §5–§6 |
| `elligator2.json` | dirty ephemeral keys, representatives, forward map, RFC 9380 cross-check, ECDH check | wire.md §2.2 |
| `noise3.json` | masking key, ephemeral, selector, key schedule, a 1300-byte Init datagram, three data packets | wire.md §2.3–§2.6 |
| `reality2.json` | REALITY v2 token for a synthetic ClientHello | wire.md §3.2 |
| `signed-object.json` | envelope with the RFC 8032 key: a release manifest and an empty-body object | objects.md §1–§2, §9 |
| `node-card.json` | a server card with four suites, two masking keys and an exit offer | objects.md §3–§4 |
| `link.json` | contact and invitation links, web form | objects.md §10 |
| `handshake.json` | Noise initial `h`/`ck` for both suites, prologue, pre-message hash, payload encodings, access proof, puzzle; inputs for the deferred transcripts | handshake.md |
| `kdf.json` | session secret, `sid`, path binding, a sealed relay record, chat/2 seal key, mailbox/2 forward key, call/2 keys, DHT keys | handshake.md §9, wire.md §8, services.md, dht.md |

## Deferred

Full XKhfs and XXhfs transcripts (every message, every intermediate `h`, `ck` and key) are **to be
produced with the implementation in step 2** (handshake.md Decision D15). `handshake.json` fixes the
inputs to use (`deferred_transcripts`) and already contains the stdlib-derived values of those inputs
(public keys, the ML-KEM encapsulation key hash, ciphertext hash and shared secret), so two independent
implementations can produce and compare the transcripts.
