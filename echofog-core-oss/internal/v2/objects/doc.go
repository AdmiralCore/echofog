// Package objects implements the protocol v2 signed objects (docs/protocol/v2/objects.md): the
// envelope, canonical protobuf bodies, node cards, device lists, prekeys, mailbox hints, name
// bindings, release manifests and links.
//
// Received objects are verified over and forwarded as the exact bytes received; nothing here
// re-encodes a received object. Bodies are read with the generated types of package pb
// (pb/objects.proto, generated with `buf generate --path internal/v2/objects/pb`); the canonical
// rules of objects.md §2.3 are checked on the bytes, driven by the generated descriptors, so fields
// a descriptor does not know are checked only for order, shortest varints and the absence of groups.
//
// # Readings of the specification
//
// Where the specification is ambiguous or contradicts the vectors, this package follows the vectors
// and the readings below (spec issues to resolve in docs/protocol/v2):
//
//   - DHT key of the release manifest: objects.md §9 says SHA-256("echofog/dht2/release");
//     registries.md §9, dht.md §2 and kdf.json add u32(day). ReleaseDHTKey follows kdf.json.
//   - role = server and camouflaged addresses (§3.2): node-card.json's invitation card is a server
//     card with only a noise3 address and no flag domestic-only. The rule is not applied to
//     invite-scope cards.
//   - "ExitOffer exit: present only with flag exit" (§3.1): the same invitation card carries an
//     offer without flag exit. An offer without the flag is valid and not live (Card.ExitOffer
//     returns nil); flag exit without an offer is invalid.
//   - registries.md §8 says every key in an object is an akey; objects.md gives Prekeys.spk (32)
//     and Prekeys.kem (1184) as raw keys. The objects.md sizes are used.
//   - echofog/mailbox-hint/2 allows a 1024-byte body, but Hint.relay_card is a whole card (the
//     vector cards are 576–807 bytes, the limit 4096): at most one small relay card fits.
//   - echofog/name/2 has no envelope limit in registries.md §9 (it is not stored); only the
//     512-byte body limit is applied.
//   - The card lifetime depends on the body (scope), so the generic step 4 uses 30 days for
//     echofog/card/2 and VerifyCard applies 7 days unless scope = invite (not the flag).
//   - Scope/flag consistency is not stated as a validity rule. Implemented: a public card has
//     neither unlisted nor invite-scoped; an invite card has an 8-byte invite_id and both flags;
//     invite_id and invite-scoped appear only in invite cards. An unknown scope value verifies,
//     gets no public-scope restrictions and is never publishable.
//   - Release deprecated_after ("treated as below min_supported") is read as: after
//     deprecated_after, versions below recommended count as below min_supported. Never-lock-out
//     still applies (no refusal of the node's own version; deprecations sooner than 90 days after
//     released_at are ignored).
//   - "A single manifest may raise min_supported by at most one major version": relative to the
//     last applied manifest's effective value (proto: 200 when none), one major = 100 for proto
//     and 1 for every other component; a larger raise is clipped, not rejected.
//   - Release next_keys "≤ 3" is a processing limit (first 3 used), like every count limit;
//     revoked keys and co-signatures with unknown tags or bad signatures are skipped.
//   - A device list may be empty (signed-object.json signs an empty devices/2 body); no card can
//     be valid with it. Device entries with unknown key tags are skipped.
//   - §2.2 orders "lengths consistent" before "alg known", but the lengths depend on the
//     algorithm: an unknown algorithm is reported as unusable before the length check.
//   - Links: a link with s must carry an invite card whose invite_id = pass_id(S); a link without s
//     must carry a public card. Not stated, implied by §10.2–§10.3 and handshake.md §5.
//   - Ed25519 signer keys with a non-canonical point encoding are rejected along with small-order
//     keys.
//   - The generic 2048-byte limit is applied to every unknown label, not only to labels starting
//     with "echofog/".
//   - "Every masking key is referenced by at least one address" counts all addresses; the processed
//     view (Card.Addrs) drops addresses whose masking key lies beyond the first 8.
//   - Cards with proto_ver < 200 are invalid (versioning.md §1 MinProtocolVersion).
//   - Shop links (D17): beyond the scheme rules, user info, an empty host, whitespace, control
//     characters and backslashes are rejected; Unicode hosts are allowed (clients show punycode).
//   - The spec gives no location for the .proto files; they are in internal/v2/objects/pb.
//     ServiceOffer (handshake.md §7) and ShapeProfile (wire.md §9) are declared there as well.
package objects
