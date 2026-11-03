# L5 signed objects, node card, links

Everything a node states about itself is a **signed object**: one envelope format, one signature rule,
stored and forwarded byte-for-byte. Test vectors: [vectors/signed-object.json](vectors/signed-object.json),
[vectors/node-card.json](vectors/node-card.json), [vectors/link.json](vectors/link.json).

## 1. Algorithm-tagged keys (L7)

```
akey = alg(u8) ‖ key bytes             (length fixed by alg, registries.md §8)
```

- Ed25519: `akey = 0x01 ‖ pub(32)`, 33 bytes. X25519: `0x02 ‖ pub(32)`. ML-KEM-768: `0x03 ‖ ek(1184)`.
- Text form: `<alg name>:<base64url-nopad(key bytes)>`, e.g. `ed25519:…`. Text forms are for humans and
  logs; the wire always uses `akey`.
- **Two identity keys (Decision D35).** Every person has an **account key** (Ed25519, derived from the
  mnemonic at `m/44'/999'/account'/0'`, as v1's identity) and every installation has a **device key**
  (Ed25519, random, generated on the device and never derived from the mnemonic). The account key signs
  the list of its devices (§6); the device key signs everything else the device publishes (cards,
  prekeys).
- `node_id = SHA-256(akey)` of the **account** key (Decision D1). It is the identity contacts, links
  and chats use, and it never changes when devices are added, replaced or removed.
- `device_id = SHA-256(akey)` of a device key. It names one installation in the network: DHT position,
  routing-table entry, session admission, the responder ID bound in the handshake.
- In v2 an account has exactly one device; formats and rules already work for several.

## 2. Signed object envelope

### 2.1 Layout

| Field | Size | Content |
|---|---|---|
| `fmt` | 1 | `0x02` — envelope format of v2 |
| `label_len` | 1 | 1..64 |
| `label` | `label_len` | ASCII label from [registries.md](registries.md) §7.1 |
| `alg` | 1 | signer algorithm tag (a signature algorithm) |
| `signer` | by `alg` | signer public key (Ed25519: 32) |
| `seq` | 8 | `u64`, ≥ 1, strictly increasing per (signer, label) |
| `not_after` | 8 | `u64` unix seconds; the object is invalid after it |
| `body_len` | varint | length of `body` |
| `body` | `body_len` | canonical protobuf (§2.3) |
| `sig` | by `alg` | Ed25519: 64 |

```
sig = Sign(signer_sk, envelope[0 : offset_of_sig])
```

The signature covers every byte before it: format, label, algorithm, signer key, `seq`, `not_after`
and the exact body bytes. For the hybrid tag `ed25519+mldsa65` (reserved), `sig` is the Ed25519
signature followed by the ML-DSA-65 signature over the same input, and both MUST verify.

**Decision D7 (signature input).** The design draft wrote `Sign(label ‖ body)`. v2 signs the whole
prefix instead: the label is length-prefixed (no ambiguity between label and body), and the algorithm,
key, `seq` and expiry are covered, so none can be swapped without breaking the signature.

**Decision D8 (seq and expiry in the envelope).** `seq` and `not_after` are envelope fields, not body
fields, so the DHT and caches can order, replace and expire objects of labels they do not understand
(K3: v1's DHT rejected unknown record kinds).

### 2.2 Verification

A receiver MUST check, in this order, and reject the object on the first failure:

1. `fmt = 0x02`; `label_len` in 1..64.
2. `alg` is a known signature algorithm. Unknown: the object is *unusable* (skipped), not malformed.
   (This comes before the length checks because the key and signature lengths depend on `alg`.)
3. Lengths consistent; total length within the label's envelope limit ([registries.md](registries.md)
   §7.1, §9); for **any** unknown label (whatever its prefix), the generic limit of 2048 bytes.
4. Signature: Ed25519 per RFC 8032 §5.1.7 with the canonical-`S` check (`S < L`). The signer key MUST
   be a canonical point encoding (y < p) and MUST NOT be of small order. Implementations MUST NOT verify
   over a re-encoding.
5. `not_after > now − 60` (60 s skew) and, for a known label, `not_after − now ≤ max lifetime + 60`,
   where the maximum is the label's largest (for `echofog/card/2` 30 days; the scope limit is applied
   in step 7).
   For an **unknown** label the object is not rejected for a long lifetime: a store treats it as
   expiring at `min(not_after, receipt + 24 h)` (it is clipped, so objects of labels introduced later
   are still stored and forwarded by older nodes).
6. `body` is canonical (§2.3) and parses as the label's message.
7. Label-specific rules (sections below).

**Schemas.** The message definitions in this specification are normative. Implementations keep a
machine-readable copy; the reference one is `echofog-core-oss/internal/v2/objects/pb/objects.proto`,
which also declares `ServiceOffer` ([handshake.md](handshake.md) §7) and `ShapeProfile`
([wire.md](wire.md) §9). Where the two differ, this text wins.

Objects are stored and forwarded as the exact received bytes. `object_id = SHA-256(envelope)`
identifies a specific object; two objects with the same `(signer, label, seq)` and different bytes are
**equivocation**. Every receiver, DHT nodes included, keeps the one with the smaller `object_id`
(one deterministic rule, so all honest nodes converge on the same object), and treats the signer as
misbehaving.

### 2.3 Canonical protobuf

A body is canonical when:

1. fields appear in strictly ascending field-number order; the elements of one repeated field are
   consecutive;
2. a non-repeated field appears at most once;
3. scalar fields with the default value and empty strings and bytes are omitted. An embedded message
   field is present exactly when the message is present, even if it is empty (`ExitOffer exit` with all
   fields zero is a free exit, not an absent one); elements of repeated fields are never omitted;
4. repeated numeric scalar fields are packed;
5. all varints (tags, lengths, values) use the shortest encoding;
6. groups (wire types 3 and 4) do not occur;
7. nested messages of known types are canonical by the same rules.

Unknown fields are allowed, kept byte-for-byte and covered by the signature. For unknown fields a
receiver checks only rules 1 (ascending field numbers; equal numbers only consecutively, since the
field may be repeated), 5 and 6. A receiver MUST reject a body that breaks a rule it can check (P4: one
object, one encoding).

**Extension rules for all object bodies.** Readers MUST ignore unknown bits of flag fields and treat an
unknown enum value (for example a `role` value) as the documented default ("unknown": for `role`, an
ordinary user). Count limits on repeated fields are **processing** limits: a reader uses the first N
elements and ignores the rest; only the byte-size limit of the envelope is a validity limit.

## 3. Node card (`echofog/card/2`)

### 3.1 Body

```
message NodeCard {
  uint32 proto_ver = 1;                 // 200
  bytes  hs_key = 2;                    // akey (x25519): the Noise static key
  repeated uint32 l1_suites = 3;        // L1 suites accepted, preference order
  repeated Addr addrs = 4;              // ≤ 16
  repeated MaskKey mask_keys = 5;       // ≤ 8
  uint32 role = 6;                      // registries.md §11
  uint32 flags = 7;                     // registries.md §11
  repeated ServiceOffer services = 8;   // ≤ 16; same message as in HsInit
  repeated uint32 features = 9;         // ≤ 64
  ExitOffer exit = 10;                  // §4; live only with flag `exit`
  repeated string shop = 11;            // ≤ 3, each ≤ 200 bytes (§3.5)
  string release = 12;                  // build version, ≤ 32 bytes, e.g. "0.3.0"
  bytes  account = 13;                  // akey of the account key; REQUIRED
  bytes  prekeys_hash = 14;             // SHA-256 of the current prekeys/2 envelope; optional
  HsKey  prev_hs = 15;                  // previous handshake key during a rotation (handshake.md §7.1)
  uint32 scope = 16;                    // 0 public, 1 peer, 2 invite (§3.2)
  bytes  invite_id = 17;                // scope invite: the invitation's pass_id (8 bytes)
  bytes  devices = 18;                  // the account's current devices/2 envelope; REQUIRED
}

message HsKey {
  bytes  key = 1;                       // akey (x25519)
  uint64 not_after = 2;
}

message Addr {
  uint32 suite = 1;                     // L0 suite, registries.md §6.1
  string host = 2;                      // IP literal or DNS name (≤ 253); empty when url is used
  uint32 port = 3;
  string url = 4;                       // ws2: wss:// URL (≤ 512)
  uint32 mask = 5;                      // 1-based index into mask_keys; REQUIRED for every v2 suite
  bytes  params = 6;                    // SuiteParams, canonical protobuf
  uint32 flags = 7;                     // bit 0: bootstrap (accepts XX handshakes); others 0
}

message MaskKey {
  bytes  key = 1;                       // akey (x25519, 33 bytes): tagged like every other key (L7)
  uint64 not_after = 2;                 // upper bound of acceptance; ≤ card not_after + 7 days
}

message SuiteParams {
  string sni = 1;                       // reality2-*, ws2: cover / inner SNI (≤ 253)
  ShapeProfile shape = 2;               // wire.md §9
}
```

### 3.2 Rules

- The envelope signer is the **device** key; `device_id = SHA-256(signer akey)`. The card is valid only
  if `devices` is a valid `echofog/devices/2` object signed by the key in `account` and lists the
  signer. Then `node_id = SHA-256(account)`. A receiver keeps the newest valid device list per account
  (highest `seq`) and treats a device missing from it as no longer belonging to the account.
- `proto_ver` MUST be ≥ 200; a card with a lower value is invalid.
- Lifetime (`not_after − now`) at most **7 days** for scopes `public` and `peer`, at most **30 days**
  for scope `invite` (Decision D12); the limit depends on the `scope` field, not on a flag. A node
  republishes when less than 2 days remain or anything changes.
- `seq` SHOULD be `max(previous seq + 1, issue time in unix seconds)`; one counter per node covers all
  scopes.
- **Scopes.** A node issues cards of three scopes:
  - `public` (0): the card stored in the DHT and passed in PEX. It MUST NOT contain `noise3` addresses
    or the masking keys they use, and SHOULD NOT contain `ws2` addresses (wire.md §2.1, §4.4).
  - `peer` (1): sent only inside sessions (`HsResp.card`, `HsFinish.card`, `CARD_UPDATE`) to peers the
    node chooses; may contain everything. Receivers MUST NOT forward or publish it.
  - `invite` (2): delivered in an invitation link (§10.3); carries `invite_id`; never forwarded.
- **Scope and flag consistency** (validity rules):
  - a `public` card has neither `unlisted` nor `invite-scoped` and no `invite_id`;
  - an `invite` card has an 8-byte `invite_id` and both `unlisted` and `invite-scoped`;
  - `invite_id` and `invite-scoped` appear only in `invite` cards;
  - an unknown `scope` value is valid, gets none of the public-scope permissions, and is never
    published or forwarded.
- **Exit offer and flag.** `exit` present without flag `exit`: valid, the offer is not live (ignored).
  Flag `exit` without `exit`: invalid.
- A receiver keeps, per (device ID, scope, `invite_id`), only the valid card with the highest `seq`, and
  never merges fields of different cards. To dial, it uses the most recent of its cards for that node.
  SNI, role, addresses and masking keys come **only** from cards (K4).
- A `public` card with `role = server` MUST list at least one camouflaged address (`reality2-*` or
  `ws2`), unless it has flag `domestic-only` (then it is never dialed across a border;
  [wire.md](wire.md) §1). `peer` and `invite` cards are exempt: they reach peers and invitees who may
  use noise3 (wire.md §2.1).
- `flags.unlisted`: receivers MUST NOT pass the card to third parties (PEX, DHT `NODES` replies) and
  MUST NOT publish it. Hidden and anonymous nodes set it; they issue no public card.
- `hs_key` and `prev_hs` MUST NOT be bound to another device ID (handshake.md §7).
- Every address with a v2 suite has a valid `mask` index; every masking key is referenced by at least
  one address (counting all addresses). In the processed view (the first 8 masking keys, §2.3
  extension rules), addresses whose key lies beyond the first 8 are dropped.
- Size: the encoded envelope is at most 4096 bytes.

### 3.3 How a card travels

Verbatim, as the same bytes, in all of: invitation and contact links (§10), bootstrap lists, PEX
`CARDS`, DHT `NODES` and `VALUE` replies, `HsResp.card`, `HsFinish.card`, `CARD_UPDATE`. A card
received in any of these is verified before use; an unverifiable card is dropped silently.

### 3.4 Multi-device

v2 ships one device per account, with the full two-level structure in place (Decision D35):

- each device has its own device key, card, handshake key, masking keys and prekeys object (bundles
  per device, not per person);
- the account key signs the device list (`echofog/devices/2`, §6), which every card embeds;
- a verifier accepts a device as the account's only if the account's current device list contains the
  device key;
- restoring the mnemonic on a new installation creates a **new device** (new random device key) that
  the account signs into the list; the old device can be removed from it. The node ID stays the same;
- chat/2 encrypts to the device named by the prekeys it uses; fan-out to several devices of an account
  is specified when multi-device ships, without any change of node IDs.

### 3.5 Shop links

`shop` lets an exit operator point to a page where people can buy access or contact them.

**Decision D17 (shop links).** Only `https://` URLs (including `https://t.me/…`) and
`echofog://contact?…` links are valid; anything else invalidates the card. Also invalid: user
information (`user@`), an empty host, whitespace, control characters or backslashes anywhere in the
link. Unicode host names are allowed (clients show them in punycode). A client:

- shows a shop link only with an "external resource, run by the node's operator" label, and shows the
  registrable domain in punycode (no look-alike Unicode);
- hides links whose domain resembles the project's own domains;
- warns that payments made outside the TON vault are not protected by EchoFog;
- shows it only for nodes whose valid cards it has seen for at least 14 days and whose local rating —
  which counts vault-paid traffic that was actually delivered, not only age — is at or above the
  client's threshold;
- never opens, fetches or previews the link automatically.

### 3.6 CARD_UPDATE

With feature `card-push`, either side may send a newer card of itself on the control stream. The
receiver applies the normal rules (higher `seq` wins).

## 4. Exit offer

**Decision D6 (exit offer inside the card).** The exit offer is a sub-message of the card, not a
separate signed object: one description of a node (V6), one `seq`, one signature. A price change is a
new card.

```
message ExitOffer {
  uint64 price = 1;                     // micro-USDT per MiB of tunnel traffic
  uint32 free_kib = 2;                  // free test traffic per client per day
  uint32 upload_kbps = 3;               // operator's claimed capacity
  string region = 4;                    // ISO 3166-1 alpha-2, upper case
  uint32 policy = 5;                    // bit0 TCP, bit1 UDP, bit2 port 25 blocked, bit3 DNS via DoH/DoT
  uint64 relay_price = 6;               // micro-USDT per MiB for relay/2; 0 = free
  repeated uint32 assets = 7;           // 1 USDT on TON, 2 TON
}
```

Not in the offer, unlike v1 (K6): neighbour lists, payment addresses (sent inside exit/2 to served
clients only), SNI outside the signed address list.

## 5. Prekeys (`echofog/prekeys/2`)

The chat/2 PQXDH bundle ([services.md](services.md) §4), signed by the identity key through the
envelope, so the v1 bundle signatures (`ECHOFOG_PREKEY_BINDING_V1/V2`) are retired.

```
message Prekeys {
  bytes  ik = 1;                        // akey (x25519): X3DH identity DH key
  bytes  spk = 2;                       // signed prekey, raw X25519, 32 bytes (fixed by the label)
  uint64 spk_created = 3;
  bytes  kem = 4;                       // raw ML-KEM-768 encapsulation key, 1184 bytes; REQUIRED
  uint64 kem_created = 5;
  repeated bytes opks = 6;              // one-time prekeys, 32 bytes each, ≤ 32; absent in DHT copies
}
```

- `ik` is the device's X3DH identity DH key, X25519, random per device (v1 derived it at index 1' of
  the account; v2 keeps that path unused so that every device has its own).
- The envelope signer is the device key, which the account's device list vouches for.
- KEM key ID = `SHA-256(kem)`, as in v1. SPK rotates every 7 days and old ones are kept 35 days; the
  KEM prekey likewise.
- Lifetime ≤ 30 days. Every v2 bundle carries `kem`; a bundle without it is invalid, which removes the
  v1 downgrade window (audit item P1).

## 6. Device list (`echofog/devices/2`)

```
message DeviceList {
  repeated Device devices = 1;          // ≤ 16
}
message Device {
  bytes  key = 1;                       // akey of the device identity
  uint64 added = 2;
  uint32 flags = 3;                     // bit 0: primary
  string label = 4;                     // ≤ 32 bytes
}
```

Signed by the account key; lifetime ≤ 30 days; republished when less than 10 days remain. An empty
list is a valid object, but no card can be valid with it; entries with unknown key tags are skipped.
Required
from v2.0: every card embeds the current list, and the list is also stored in the DHT under the
account's node ID. In v2.0 it lists exactly one device, flagged primary.

## 7. Mailbox hint (`echofog/mailbox-hint/2`)

A person's public first-contact queues ([services.md](services.md) §5). Contacts get private queues
inside end-to-end messages instead.

```
message MailboxHint {
  repeated Hint hints = 1;              // ≤ 4
}
message Hint {
  bytes  relay = 1;                     // device ID of the mailbox relay
  bytes  queue = 2;                     // 16-byte public queue ID
  uint64 not_after = 3;
  bytes  relay_card = 4;                // optional: the relay's card (unlisted relays); at most one per object
}
```

Lifetime ≤ 6 hours. The body is at most 6144 bytes, so it fits up to 4 hints and at most one relay
card (cards are at most 4096 bytes).

## 8. Name binding (`echofog/name/2`)

A person's claim to a blockchain name (`alice.ton`). The format is defined here; resolution against the
chain is specified with the `names` service later. Limits: body ≤ 512 bytes, envelope ≤ 768 bytes.
**v2 nodes do not store or serve this kind in the
DHT** ([dht.md](dht.md) §2): without an on-chain check, any signer could squat a name.

```
message NameBinding {
  string name = 1;                      // normalised name
  uint32 system = 2;                    // 1 TON DNS, 2 Telegram usernames (t.me)
  bytes  owner = 3;                     // the name owner's account when signed (≤ 64 bytes)
  uint64 issued = 4;
}
```

## 9. Release manifest (`echofog/release/2`)

The object body carries the manifest and co-signatures:

```
message ReleaseObject {                 // body of echofog/release/2
  bytes manifest = 1;                   // canonical Release message
  repeated CoSig cosigs = 2;
}
message CoSig {
  bytes key = 1;                        // akey of a release key
  bytes sig = 2;                        // Sign(key, "echofog/release-cosign/2" ‖ SHA-256(manifest))
}

message Release {
  string version = 1;                   // "0.3.0"
  uint64 released_at = 2;
  repeated Policy policies = 3;
  repeated Artifact artifacts = 4;
  repeated bytes next_keys = 5;         // akeys to add to the release key set, ≤ 3
  string notes_url = 6;                 // https only
  repeated bytes revoked_keys = 7;      // akeys removed from the release key set
}
message Policy {
  string component = 1;                 // "proto", "l1:1", "svc:3", "obj:echofog/card/2", "l0:2"
  uint32 min_supported = 2;
  uint32 recommended = 3;
  uint64 deprecated_after = 4;
}
message Artifact {
  string platform = 1;                  // "windows-amd64", "linux-arm64", "android-arm64", …
  string name = 2;
  bytes  sha256 = 3;
  uint64 size = 4;
  string url = 5;                       // https only
}
```

**Decision D16 (release keys, revised).** Builds carry 3 release public keys (the initial key set). The
project maintainer holds the three private keys, each on separate offline media.

- **Threshold.** A manifest is valid only with signatures by **2 distinct keys** of the current key set:
  the envelope signer plus at least one `CoSig` over the same manifest. Release keys are never node
  keys.
- **Key set.** The current set is the compiled set, plus `next_keys`, minus `revoked_keys`, of every
  applied manifest in order. A manifest that changes the set must be valid under the set before the
  change, and its 2 signers must not be among the keys it revokes. The set never has more than 5 keys
  and never fewer than 2.
- **seq.** The envelope `seq` MUST equal `released_at`; a reader rejects a manifest with
  `released_at > now + 86400` or `released_at` ≤ that of the last applied manifest. A manifest cannot
  block later ones with a huge `seq`.
- **Discovery.** Manifests are stored in the DHT under the key
  `SHA-256("echofog/dht2/release" ‖ u32(day))`, independent of the signing key ([dht.md](dht.md) §2),
  so rotated keys are found by old builds; servers also return the latest manifest in pex/2.
- **Counts.** `next_keys` ≤ 3 is a processing limit (the first 3 are used). Revoked keys and
  co-signatures with unknown algorithm tags or bad signatures are skipped, not errors.
- **`deprecated_after`.** After `deprecated_after`, versions of the component below `recommended`
  count as below `min_supported` (still subject to the never-lock-out rules).
- **One major version.** A manifest may raise `min_supported` of a component by at most one major
  version relative to the last applied manifest's effective value (for `proto`, 200 when none was
  applied). One major version is 100 for `proto` (200 → 300) and 1 for every other component. A larger
  raise is clipped to that step, not rejected.
- **Never lock out.** Rules in [versioning.md](versioning.md) §5.
- **Notify only.** Artifacts are information for the user and for package managers. A client shows
  that an update is available (and "required" below `min_supported`) with the version, the notes URL
  and the artifact hashes; it never downloads or installs an artifact by itself.

## 10. Links v2

### 10.1 Format

```
echofog://join?v=2&card=<card64>[&s=<secret64>]

card64   = base64url-nopad( method(u8) ‖ data )
           method 0x00: data = the signed card envelope
           method 0x01: data = raw DEFLATE (RFC 1951) of the signed card envelope
secret64 = base64url-nopad( S ), S = 16-byte access secret
```

- Parameters appear in the order `v`, `card`, `s`; unknown parameters are ignored; a link with
  `v` other than 2 is refused with "made by another version".
- **Link and card must match.** A link with `s` MUST carry a card of scope `invite` whose `invite_id`
  equals `pass_id(S)` ([handshake.md](handshake.md) §5); a link without `s` MUST carry a card of scope
  `public`. Any other combination is refused.
- An encoder SHOULD use the shorter of the two methods (0x00 on a tie). Decoders MUST accept both and
  MUST stop inflating after 4097 output bytes (a card is at most 4096).
- The link is complete: the first carrier can be masked and the card verified offline. No DHT lookup
  is needed to connect.
- Web form, for messengers that do not open custom schemes:
  `https://<join page>/join/#` followed by the link without `echofog://`. The fragment is never sent
  to the web server.

### 10.2 Contact link (public node)

The node's current public card, without `s`.

### 10.3 Invitation link (hidden node)

For each invitation the hidden node mints:

1. a new masking key pair, used only by this invitation (or by a small group of unredeemed invitations
   when a listener's key limit is reached, [wire.md](wire.md) §2.1);
2. a new 16-byte access secret `S` and its pass (`pass_id`, expiry, use count) in local state;
3. an **invitation card**: the node's identity, `scope = invite`, `invite_id = pass_id`, flags
   `unlisted | invite-scoped`, the addresses through which it is reachable, each pointing to the new
   masking key; lifetime ≤ 30 days (Decision D12).

Lifecycle:

- **Redemption.** In the first complete session, the hidden node gives the contact a personal pass
  ([handshake.md](handshake.md) §5) and a peer-scope card whose addresses use the node's **contacts
  masking key**. The invitation pass is then used up (if its use count allows only one entry).
- **Contacts key.** One key (per listener) shared by all contacts, rotated at least every 30 days: the
  node sends the new peer card to every contact in a session before it stops accepting the old key, and
  keeps the old one until every contact seen in the last 30 days has the new card. Sharing is safe:
  the noise3 selector is keyed on the ECDH secret, so a contact who knows the key cannot recognise
  other contacts' flows passively; at most it can probe actively, which a contact can do anyway.
- **Revoking an unredeemed invitation** deletes its pass and, when no other unredeemed invitation uses
  it, its masking key at once (regardless of `not_after`). If the key is shared with other unredeemed
  invitations, the node reissues those invitations (their holders have no session yet, so they need a
  new link).
- **Revoking a contact** deletes its personal pass; its handshakes then fail with the uniform silent
  close. Its knowledge of the contacts key lets it confirm the node actively, so after revoking a
  contact the node SHOULD rotate the contacts key early.

Because unredeemed invitations have their own masking keys, a leaked link stops working at L0 as soon
as it is revoked.
