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
| `elligator2.json` | dirty ephemeral keys, representatives, forward map, a key without a representative, RFC 9380 cross-check, ECDH check | wire.md §2.2 |
| `noise3.json` | masking key, ephemeral, ECDH-keyed selector, key schedule, a 1300-byte Init datagram, data packets with packet-number protection (including recovery after 4998 lost packets) | wire.md §2.3–§2.6 |
| `reality2.json` | REALITY v2 token for a synthetic ClientHello, bound to the dialed address; C from a hybrid share | wire.md §3.2 |
| `signed-object.json` | envelope with the RFC 8032 key: a co-signed (2 of n) release manifest and an empty-body object | objects.md §1–§2, §9 |
| `node-card.json` | account key, device key and the account-signed device list; one server's public, peer and invitation cards signed by the device key (noise3 only outside the public card) | objects.md §3–§4, §10.3 |
| `link.json` | contact link (public card) and invitation link (invite card and secret), web form | objects.md §10 |
| `handshake-transcript.json` | **normative** XKhfs, XKhfs with a live access proof (hidden node), and XXhfs transcripts: every message, every step's `h`/`ck`/`k`/`n`, final keys; application-valid payloads and cards | handshake.md §12 |
| `handshake.json` | Noise initial `h`/`ck` for both suites, prologues with and without a bound responder ID, pre-message hash, payload encodings with GREASE, access proof, puzzle with HS_RETRY; inputs for the deferred transcripts | handshake.md |
| `kdf.json` | session secret, `sid`, path binding, a sealed record, relay circuit binding and rendezvous ID, a full chat/2 sealed envelope, a sealed mailbox/2 forward and public-queue proof of work, call/2 keys, two media units and their CALL_AUTH batch authenticator, the chat/2 PQXDH session key under `echofog/chat2/pqxdh`, day-keyed DHT keys for a card and a device list | handshake.md §9, wire.md §8, services.md, dht.md |
| `negative.json` | inputs that MUST be rejected: non-canonical bodies, `S ≥ L`, a small-order signer, out-of-window and wrong-address first packets | objects.md §2, wire.md §2–§3 |

## Transcripts

`handshake-transcript.json` is produced by the generator's own Noise implementation (`gen/noise.go`)
It reproduced byte for byte the first transcripts of the two independent step-2 implementations
(which used the earlier, partly invalid payloads); the current transcripts use application-valid
inputs and must now be reproduced by both implementations again. To check
another implementation's output: `GOWORK=off go run . -crosscheck <its handshake-transcript.json>`
recomputes every transcript from that file's inputs and compares the message bytes and final hash.
