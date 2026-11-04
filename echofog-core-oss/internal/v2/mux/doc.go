package mux

// Where the rules of this package come from. The step-2 readings S1–S22 are settled in the
// specification (CHANGES-from-v1.md §6); each rule below is implemented as written there.
//
//	Receive rules for types   0x00, 0x3f, GREASE 0x3e and unassigned types are unknown: skipped
//	                          with IGNORABLE, else CLOSE{unknown-frame} (stream), GOAWAY{2}
//	                          (stream 0) or carrier close (mux connection level); never abuse.
//	                          Known types out of place are protocol violations and may count as
//	                          abuse; feature frames without their feature are unknown.
//	                          wire.md §5.2, §6.1, §6.2; registries.md §1.
//	Reserved flag bits        OPEN, MUX_STREAM: sent as 0, ignored on receipt. wire.md §5.2, §6.3.
//	Mux streams               opened in ID order and announced at once; a lower finished ID is
//	                          discarded; 256 per side per connection and per session (refused,
//	                          not abuse); no unidirectional streams; data before MUX_RESET stays
//	                          readable. wire.md §5.2.
//	UNSUPPORTED               ends with FIN; selected pairs usable both ways. wire.md §6.3,
//	                          services.md §1.
//	CLOSE on stream 0         ends that path. wire.md §6.3.
//	GOAWAY                    closes when the streams of both sides end, or after 15 s. wire.md §7.3.
//	Paths                     PATH_BIND sent by the carrier client, either role creates paths;
//	                          60 s without any path ends the session. wire.md §8, §8.1, §8.2.
//	Liveness and switching    every path, standby included, PINGs after uniform(12, 24) s idle
//	                          (low_power: uniform(40, 55) s) and is dead after 60 s silent; the
//	                          active path is failing on a PING without PONG within
//	                          max(1 s, 3 × RTT), or 10 s without receipt while data or PINGs are
//	                          outstanding, and is then replaced by the best standby; at most 2
//	                          standby paths, none with low_power. wire.md §7.1, §8.3, §10;
//	                          platforms.md §2. Because the mux has no acknowledgements, data sent
//	                          without any answer for half of the 10 s triggers a probe PING, whose
//	                          PONG deadline then decides.
//	Sealed records            AAD sid ‖ u64(n) ‖ u8(class) ‖ varint(path_id) ‖ varint(pipe);
//	                          ordered records: strictly growing n per pipe, no window; SEALED
//	                          datagrams: window of 4096 over datagram counters; u16 length value
//	                          ≤ 16384 + 64 without the prefix; nested or misplaced SEALED
//	                          datagrams dropped and counted. wire.md §8.4.
//	Sealed mode               a byte-stream carrier is one pipe (pipe 0): records wrap the whole
//	                          connection-level frame sequence after the handshake, or after
//	                          PATH_BIND_OK on a bound path; on carriers with native streams each
//	                          stream is its own pipe (pipe = stream ID). wire.md §8.2, §8.5.
//
// Sealed mode on a byte-stream carrier switches from plain frames to records mid-connection. To
// find the switch point without signalling, a sealed-mode Conn reads one frame at a time and only
// while stream 0 is waiting for bytes (or, on the carrier server, before stream 0 exists); the
// session calls StartSealing when the handshake or PATH_BIND exchange is complete.
