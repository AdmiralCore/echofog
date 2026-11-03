// Package handshake implements the L1 handshake of EchoFog protocol v2
// (docs/protocol/v2/handshake.md): HS_HELLO and suite selection, the prologue with channel and
// responder-ID binding, the XKhfs/XXhfs Noise exchange (package internal/v2/noise), the P1/P2/P3
// payloads, the hidden-node access proof, the HS_RETRY puzzle, 0-RTT data, rejections and the
// session derivations.
//
// The package is a pure state machine: it reads and returns frame payloads and never touches a
// socket or a timer. The carrier code frames them (HS_HELLO 0x02, HS_MSG 0x03, HS_RETRY 0x04,
// REJECT 0x05 on stream 0), applies the timeouts of §11 and performs the closes a Reply asks for
// (ActSilentClose: close at Reply.CloseAt, without a message). It is not wired into the v1 node.
//
// # Readings of the specification
//
// Where handshake.md is silent or ambiguous, this implementation chose the following readings;
// each matches the vectors in docs/protocol/v2/vectors.
//
//  1. Payload encoding. P1, P2, P3 and Reject are decoded with the canonical rules of objects.md
//     §2.3 (field order, no duplicates, no default scalars, packed repeated fields, minimal
//     varints). The specification states these rules for signed bodies only.
//  2. GREASE in HS_HELLO.offered. §3.1 orders offered by preference, §3.4 says lists stay sorted
//     with GREASE "in its numeric place". The GREASE suite is inserted before the first larger
//     entry; the other entries keep their preference order.
//  3. Initiator clock check. A P2.time more than 600 s off makes the initiator abort without
//     message 3 (it has no way to send Reject{clock-skew} inside the handshake).
//  4. Early data present but not processed (feature not negotiated, outside the 60 s window,
//     replayed, not 0-RTT safe) is reported as early_status = 2.
//  5. A P1 in XX that carries access or early is treated as malformed (silent close).
//  6. Puzzle cookies: the minute is taken from the local clock at arrival; cookies made with the
//     previous daily secret R are accepted like those of the previous minute. An initiator answers
//     at most two HS_RETRY per connection attempt.
//  7. HS_HELLO with a duplicated suite in offered, or with trailing bytes, is malformed.
//
// Card checks use CardVerifier. DefaultCardVerifier covers what the handshake needs (envelope,
// RFC 8032 signature with canonical S and no small-order signer, lifetimes, the top-level body,
// hs_key, prev_hs and the embedded device list); the full object rules belong to
// internal/v2/objects, which can be plugged in through the interface.
package handshake
