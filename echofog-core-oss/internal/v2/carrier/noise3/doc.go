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
//   - Inner QUIC Initial size. carrier/quicx.Noise3QUICConfig sizes Initial packets by the data
//     overhead (22 bytes), but an Init datagram has 71 bytes of overhead. This package overrides
//     InitialPacketSize to exactly 1200 bytes (wire.md §2.4: the inner stack sends Initial
//     datagrams of exactly 1200 bytes) and keeps MaxUDPPayloadSize from the quicx config for the
//     data direction. Without the override a 1382-byte inner datagram would need a 1453-byte Init.
//   - Key sets. The dialer keeps the newest four Init key sets; the responder keeps the first
//     Init's keys (it always answers with them, §2.5 step 6) plus the three newest sibling sets.
//     A response is attributed to a set by trying the sets newest first.
//   - Replay filter. A 2 MiB Bloom filter with six hashes is flushed to KV at least every
//     uniform(8, 12) s while packets arrive, and on Close. It is not rotated: keeping an old
//     selector longer than 900 s can only drop a packet whose selector repeats, which is a
//     collision of a keyed HMAC and not a predictable event.
//   - Retry. The inner QUIC transport requires address validation (a QUIC Retry inside noise3)
//     when pending flows or total flows reach 75% of their limits. The specification says SHOULD.
//   - Random eviction. math/rand/v2 drives eviction and padding, never keys: keys and Elligator
//     tweaks come from crypto/rand. The eviction candidate is chosen uniformly from the pending
//     flows (or from all flows when no pending flow exists).
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
