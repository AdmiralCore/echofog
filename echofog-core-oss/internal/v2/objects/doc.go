// Package objects implements the protocol v2 signed objects (docs/protocol/v2/objects.md): the
// envelope, canonical protobuf bodies, node cards, device lists, prekeys, mailbox hints, name
// bindings, release manifests and links.
//
// Received objects are verified over and forwarded as the exact bytes received; nothing here
// re-encodes a received object. Bodies are read with the generated types of package pb
// (pb/objects.proto, the reference copy of the schemas named in objects.md §2.2, generated with
// `buf generate --path internal/v2/objects/pb`); the canonical rules of objects.md §2.3 are checked
// on the bytes, driven by the generated descriptors, so fields a descriptor does not know are checked
// only for order, shortest varints and the absence of groups.
//
// # Rules settled in the specification (CHANGES-from-v1.md §6)
//
// The points that were open during the first implementation are now normative text; the code
// follows it:
//
//   - Verification order (objects.md §2.2): format and label, then the algorithm (unknown:
//     unusable), then lengths and the envelope limit, then the signature (canonical S; the signer
//     must be a canonical point encoding and not of small order), expiry and lifetime, canonical
//     body, label rules. Every unknown label, whatever its prefix, has the 2048-byte limit and no
//     lifetime limit (stores clip it to 24 h).
//   - Limits (registries.md §7.1, §9): mailbox-hint body 6144, envelope 6400, at most one relay card
//     per object; name binding body 512, envelope 768.
//   - Cards (objects.md §3.2): proto_ver ≥ 200; lifetime by the scope field (7 days public and peer,
//     30 days invite; unknown scopes 7 days); scope/flag consistency rules; unknown scopes valid but
//     never publishable; the camouflage rule for role = server applies to public cards only; an
//     exit offer without flag exit is valid and not live (Card.ExitOffer returns nil), flag exit
//     without an offer is invalid; every masking key is referenced by some address (counting all
//     addresses) and the processed view drops addresses whose key lies beyond the first 8.
//   - Shop links (D17, §3.5): https:// and echofog://contact?… only; no user info, empty host,
//     whitespace, control characters or backslashes; Unicode hosts allowed.
//   - Prekeys.spk and Prekeys.kem are raw keys by definition (registries.md §8 exceptions).
//   - Device lists (§6): an empty list is a valid object but no card is valid with it; entries
//     with unknown key tags are skipped.
//   - Release manifests (§9): DHT key SHA-256("echofog/dht2/release" ‖ u32(day)); next_keys ≤ 3 is
//     a processing limit; revoked keys and co-signatures with unknown tags or bad signatures are
//     skipped; after deprecated_after, versions below recommended count as below min_supported; a
//     min_supported raise is clipped to one major version (100 for proto, 1 otherwise) over the last
//     applied effective value (proto: 200 when none); never-lock-out per versioning.md §5.
//   - Links (§10.1): a link with s carries an invite card whose invite_id = pass_id(S); a link
//     without s carries a public card; anything else is refused.
//
// # Remaining implementation choices
//
//   - Card lifetime for unknown scope values is 7 days (the text names only public, peer, invite).
//   - CardBook implements the §3.2 receiver rules (highest seq per slot, equivocation by smaller
//     object_id, hs_key bound to one device, newest device list per account) as an in-memory helper;
//     it is not wired into a node.
package objects
