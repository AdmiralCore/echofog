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
// # Spec decisions followed
//
// handshake.md (step-2 clarifications, CHANGES-from-v1.md §6) settles the points this package
// first had to read for itself: P1/P2/P3/Reject are canonical protobuf (objects.md §2.3); a
// GREASE suite goes before the first larger entry of offered and is never chosen; the P2 card is
// the current card of the scope named by known_card_seq, sent exactly when its seq is higher, never
// for an invitation card, and XX always carries the public card; the responder tries the current
// hs key, then a full second pass with the previous one; rid is a device ID; hidden nodes accept
// XK only; ck is erased after Split, session_secret and sid; a P1 in XX with access,
// known_card_seq or early is malformed; an initiator answers at most 2 HS_RETRY; the previous R
// is accepted during the minute after rotation; a skewed P2.time makes the initiator abort
// without message 3; unprocessed early data is early_status 2.
//
// The normative vectors/handshake-transcript.json is reproduced byte for byte both by the
// Noise layer and by the Initiator/Responder state machines with all checks of §7
// (TestTranscriptVectors, TestApplicationTranscripts).
//
// Card checks use CardVerifier. DefaultCardVerifier covers what the handshake needs (envelope,
// RFC 8032 signature with canonical S and no small-order signer, lifetimes, the top-level body,
// hs_key, prev_hs and the embedded device list); the full object rules belong to
// internal/v2/objects, which can be plugged in through the interface.
package handshake
