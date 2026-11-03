package mux

// Spec readings. Where docs/protocol/v2 is silent or ambiguous, the frame, mux and service
// packages implement the reading below; each is a spec issue to settle in the text.
//
//	S1  varint.json "8000000025" is five bytes: a non-minimal 4-byte varint plus one byte. The
//	    varint is rejected; the extra byte plays no part.
//	S2  Types 0x00 and 0x3f ("reserved, invalid") have no receive rule: they are unknown types
//	    (skipped with IGNORABLE, otherwise CLOSE{unknown-frame}), never abuse.
//	S3  GREASE 0x3e is never a known type on receipt. At mux connection level, where §5.2 lists
//	    only MUX_* and PADDING, unknown IGNORABLE frames (GREASE included) are skipped; an unknown
//	    frame without IGNORABLE closes the carrier (there is no stream to CLOSE), not abuse.
//	S4  A path silent for 10 s is abandoned while another lives (§8.3), but keepalives come every
//	    12–24 s, so an idle standby path never survives. Only non-active paths are abandoned.
//	S5  Reserved flag bits of OPEN and MUX_STREAM ("MUST be 0") have no receive rule: ignored.
//	S6  "Records are at most 16384 + 64 bytes": taken as the bound on the u16 length value
//	    (kind + body), the 2-byte prefix excluded.
//	S7  The shared 4096 replay window (§8.4) meets records inside mux streams that are opened
//	    when the application reads (sealed mode, §8.5): a slow reader would see its records fall
//	    out of the window. Records of one ordered stream must have growing counters and use the
//	    window while inside it; datagrams and relay circuit records use the window strictly.
//	S8  UNSUPPORTED "and the stream ends": the stream ends with FIN, not CLOSE + reset (a reset
//	    may discard the UNSUPPORTED on QUIC).
//	S9  The handshake selects services the responder serves; streams the responder opens to the
//	    initiator are not covered. A selected pair is usable both ways; a side without a handler
//	    answers UNSUPPORTED{service, 0, 0}.
//	S10 Mux stream IDs "increase by 2" but a stream opens with its first MUX_STREAM: a sender
//	    could use them out of order. Senders announce each stream at once (empty MUX_STREAM);
//	    receivers treat a peer ID below the highest seen as finished.
//	S11 The mux has no unidirectional streams (no direction bit in its IDs), so the limit of 64
//	    unidirectional streams (§10) applies to QUIC carriers only.
//	S12 "At most 256 streams open per side" is enforced per mux connection (MUX_RESET refused)
//	    and per session (CLOSE{refused}), both never abuse.
//	S13 After MUX_RESET, data that arrived before it stays readable, so a CLOSE frame sent just
//	    before the reset (§6.3) is delivered.
//	S14 CLOSE on stream 0 (CLOSE is valid on any stream) ends that path, not the session.
//	S15 Frames introduced by a feature (OBSERVED_ADDR, CARD_UPDATE, PUNCH_*, PATH_BIND) that
//	    arrive without the feature are unknown frames (skipped if IGNORABLE, else GOAWAY{2}).
//	S16 A known session-level type on the wrong stream (PING on a service stream, OPEN on stream
//	    0) is a protocol violation and counts as abuse (misuse of a known type).
//	S17 A session whose last path died waits NoPathGrace (60 s, the path-dead time) for a
//	    PATH_BIND, then ends.
//	S18 The PATH_BIND sender is the carrier client (it opens stream 0); either session role may
//	    create paths, with the parity of its role. On a sealed-mode path, records start after
//	    PATH_BIND / PATH_BIND_OK.
//	S19 GOAWAY "closes the session when its streams end": the streams of both sides.
//	S20 kdf.json uses keys with commas ("split_k_i2r (noise HKDF(ck, empty).o1)"), which
//	    encoding/json struct tags cannot express; plain keys would help implementers.
//	S21 A SEALED datagram whose plaintext is again SEALED, or that arrives on a path not in
//	    sealed mode, is dropped and counted; a plaintext datagram on a sealed path likewise.
