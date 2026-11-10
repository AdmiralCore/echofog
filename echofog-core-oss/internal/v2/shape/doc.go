// Package shape implements the traffic shape profiles of EchoFog protocol v2
// (docs/protocol/v2/wire.md §9, registries.md §6.3): the validated profile read from a card's
// Addr.params, the built-in profiles and the per-direction generators for init lengths, padding,
// junk and decoys, keepalive and idle padding.
//
// A shape profile is data, not code. It never changes a cryptographic format: receivers accept any
// length and timing within the limits of wire.md §10, and this package only decides what a sender
// puts on the wire.
//
// # Bounds enforced when a profile is read
//
// The specification fixes some bounds directly, and the rest follow from the limits of §10 and the
// datagram layout of §2.4:
//
//   - init_len lies within [600, 1200]: no datagram of a noise3 flow is longer than 1200 bytes
//     before the responder has answered, and the inner QUIC flight needs at least 600 (§2.4).
//   - pad is at most 16384: a TLS record carries at most 2^14 plaintext bytes, and §10 caps mux
//     stream data per frame at 16384.
//   - buckets are strictly ascending, at most 32 entries, each in [1, 16384]; a bucket is a TLS
//     record size. When buckets are present they define padding and the pad range is not drawn.
//   - pad_budget is an inclusive range with min ≤ max; it is the number of bytes of one stream
//     direction during which padding applies (§9, "bytes per stream direction during which padding
//     applies"). A budget of 0 disables padding; an absent budget does not limit it.
//   - keepalive_s lies within [1, 59] seconds. §10 declares a path dead after 60 s without traffic,
//     so a keepalive at or above 60 s can never keep a path alive. An absent range means the
//     wire.md §7.1 value [12, 24] s.
//   - idle_pad_ms has min ≥ 1 and min ≤ max; a zero gap would turn idle padding into a flood.
//   - junk is limited to 16 datagrams, each at most 1452 bytes and 1000 ms apart; decoys to 16
//     entries of at most 1452 bytes. These caps are resource bounds, not wire values: without them a
//     card could make a client emit an unbounded number of datagrams.
//
// # Remaining implementation choices
//
//   - A built-in id carries the whole profile: a profile with a non-zero id and any other field set
//     is rejected (it is ambiguous whether the data overrides the table).
//   - When buckets are set, padding rounds the current size up to the smallest bucket not below it;
//     the pad range is the uniform mode used when buckets are absent (wire.md §9 "if set").
//   - A pad budget is measured in bytes of traffic, not bytes of padding, so that [4096, 16384]
//     matches "the first 4–16 KiB of each direction". A record that straddles the end of the window
//     is padded only up to the window end, and the stream is not padded afterwards.
//   - The DNS decoy is a recursion-desired A INET query with a random ID; the QUIC-Initial-shaped
//     decoy is a 1200-byte QUIC v1 long-header datagram with a random connection ID and payload.
//   - A raw decoy template replaces every maximal run of 0xff bytes with random bytes of the same
//     length; everything else in the template is copied unchanged.
//   - Idle padding is disabled under the platform `low_power` flag (platforms.md §2). Keepalive is
//     not changed here: platforms.md lowers its rate, and that policy belongs to the node.
//   - PadFor returns padding for a datagram or record; a UDP carrier clamps it to the path MTU
//     (wire.md §2.4, §9). The package does not know the path MTU and does not guess it.
package shape
