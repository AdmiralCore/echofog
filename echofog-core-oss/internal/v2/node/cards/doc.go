// Package cards builds and keeps the node's own cards (objects.md §3): one per scope, signed by
// the device key, with the addresses taken from the node's listeners and masking keys.
//
// The builder is deliberately independent of the runtime (T3.7a): it holds the card material, the
// injected clock (AD13) and a small KV view instead of the store (T4.3). The package produces
// exactly the OwnCards structure the handshake expects for P2 (handshake.md §7): the current card
// per scope and the memory of the seq and scope of every card issued within the last 30 days.
//
// Scope rules are enforced before signing:
//
//   - a public card never contains a noise3 address or the masking key of one (wire.md §2.1, D37),
//     so a node without a camouflaged listener has no public card at all;
//   - hidden and anonymous nodes issue no public card and mark their peer cards unlisted;
//   - an invitation card carries an 8-byte invite_id and both the unlisted and invite-scoped flags;
//   - the encoded body is limited to 3800 bytes by objects.SealCard (the envelope to 4096).
package cards
