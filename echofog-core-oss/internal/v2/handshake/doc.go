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
// Clean-room items B1–B4: for a known_card_seq that is 0, absent or not remembered (issued cards
// are remembered 30 days), a public responder sends its public card and a hidden one its peer card
// for a personal pass and nothing for an invitation pass; an XX P1 may carry pad; HS_HELLO with two
// GREASE values, only GREASE, or a chosen that is not the first non-GREASE entry is malformed; XX to
// a bare address requires InitiatorConfig.Throwaway (fresh account, device and hs key), and the
// card must name the throwaway hs key.
//
// Open spec issue: §3.1 says a REJECT{suite} retry sends "its full original offered list again",
// choosing the best suite of the intersection, but B3 makes a chosen that is not the first
// non-GREASE entry malformed. This package keeps the same set and moves the chosen suite to the
// front, which satisfies both rules and keeps the downgrade protection (the responder still
// requires its own best suite of the set).
//
// The normative vectors/handshake-transcript.json (four transcripts, the rotation block and the
// §8.4 sealed-record examples) is reproduced byte for byte both by the Noise layer and by the
// Initiator/Responder state machines with all checks of §7 (TestTranscriptVectors,
// TestApplicationTranscripts).
//
// Card checks use CardVerifier. DefaultCardVerifier covers what the handshake needs (envelope,
// RFC 8032 signature with canonical S and no small-order signer, lifetimes, the top-level body,
// hs_key, prev_hs and the embedded device list); the full object rules belong to
// internal/v2/objects, which can be plugged in through the interface.
package handshake
