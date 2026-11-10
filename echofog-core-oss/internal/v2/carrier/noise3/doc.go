// Package noise3 implements the masked UDP carrier of docs/protocol/v2/wire.md §2.
//
// Layout:
//
//	elligator/  Elligator2 representative of a dirty X25519 ephemeral key (§2.2)
//	packet/     key schedule, Init and data datagrams, header protection, number recovery, replay
//	            window (§2.3–§2.6)
//	noise3      Listener, Dialer, masking keys, budgets, pending flows and the virtual
//	            net.PacketConn the inner QUIC stack runs on (§2.1, §2.5–§2.7)
//
// Choices made where the specification leaves room:
//
//   - Inner QUIC Initial size (N3-FIX). No datagram of a flow before the responder's first answer
//     is longer than 1200 bytes (wire.md §2.4). The dialer draws the Init total F of a flow from
//     the profile's init_len and has the inner QUIC client send its whole handshake flight at
//     F-71 bytes (fork option InitialFlightSize, patch 4b in third_party/quic-go); both ends allow
//     Initial datagrams below 1200 (AllowSmallInitial). The inner ClientHello (X25519MLKEM768)
//     spans two or more such datagrams, each its own Init.
//   - Outer datagram ceiling. The default is 1172 bytes (the UDP payload of an IPv4 path of MTU
//     1200); a caller may pass another MaxDatagramLen between 900 and 1452. Below 900 the dialer
//     and listener fail with ErrBadConfig (the spec's ErrPathTooSmall). The inner QUIC stack
//     advertises max_udp_payload_size of the ceiling minus the data overhead and the largest
//     padding and does not discover the path MTU.
//   - Key sets. The dialer keeps the newest four Init key sets; the responder keeps the first
//     Init's keys (it always answers with them, §2.5 step 6) plus the three newest sibling sets.
//     A response is attributed to a set by trying the sets newest first.
//   - Replay filter. The cache is a set of eight rotating Bloom generations, each 2^22 bits with
//     six hashes. A generation lives for 120 s and is cleared when the rotation index wraps, so
//     the live set covers 960 s at the full 2000 Init/s listener budget and its capacity is
//     reclaimed instead of saturating forever. The worst-case false-positive rate of one
//     generation at that load is about 0.06%; the chance that any of the eight live generations
//     reports a false positive stays below 0.5%. The generations and their rotation timestamp are
//     persisted in one KV blob, flushed at least every uniform(8, 12) s and by the sweep timer.
//   - Retry. The inner QUIC transport requires address validation (a QUIC Retry inside noise3)
//     when pending flows or total flows reach 75% of their limits. The specification says SHOULD.
//   - Random eviction. math/rand/v2 drives eviction and padding, never keys: keys and Elligator
//     tweaks come from crypto/rand. Only pending flows are evicted (wire.md §2.5 step 7); if the
//     local total-flow bound is full of established flows, a new Init is dropped instead.
//   - DNS. A noise3 address may carry a DNS name, but the carrier never resolves it with the
//     local system resolver: a name is dialed only through an injected DialContext
//     (carrier.DialRequest.DialContext or DialerConfig.DialContext), otherwise Dial returns
//     ErrDNSName. IP literals are dialed directly.
//   - Virtual addresses. Every responder flow gets a stable noise3/… address for its whole life;
//     NAT rebinding does not change it. The dialer presents one local and one remote virtual
//     address to its inner QUIC stack; a dead-flow restart keeps the inner connection and starts
//     a new responder flow, which QUIC sees as a new path.
//   - Anti-amplification. A flow may send at most 3× the bytes it received until the inner QUIC
//     handshake is confirmed and an authenticated packet has arrived from the current reply
//     address. After that the cap is lifted.
//   - Junk and decoys run once before the first Init of every flow, including a restarted flow.
//
// The listener never answers an unauthenticated datagram (V1). Every failure in §2.5 is a silent
// drop.
package noise3
