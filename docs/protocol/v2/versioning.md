# Versioning

v1 had one protocol number for everything, and nothing read it. v2 versions each layer separately,
negotiates each where it belongs, and keeps every negotiation inside an encrypted channel or a
signed object. Nothing about versions is visible to an on-path observer.

## 1. The layers

| Layer | What is versioned | How peers agree | Where it is visible |
|---|---|---|---|
| L0 masking | masking suite and its parameters (`noise3`, `reality2-quic`, `reality2-tcp`, `ws2`) | the initiator picks from the responder's signed card; the responder recognises the suite by trial authentication | nowhere: random bytes or a browser-like TLS flow |
| L1 handshake | handshake suite = Noise protocol name, e.g. `EchoFog/2/Noise_XKhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s` | initiator offers a list and picks; responder enforces the best common choice; both the list and the choice are in the transcript | inside the carrier TLS only |
| L2 session | feature IDs (`u16`) | each side lists its features in the handshake; the result is the intersection | inside the handshake |
| L3 services | each service separately: `dht/2`, `chat/2`, `exit/2`, … | the handshake exchanges the service lists; every stream names its `(service, version)` in OPEN | inside the session |
| L4 messages | protobuf fields | evolution rules (§5) | inside |
| L5 signed objects | object label (`echofog/card/2`) and `seq` | label names the format; the signature covers exact bytes | in records |
| L6 storage | local database and key-file schema | schema number + forward migrations + backup before migration | local only |
| L7 cryptography | key and signature algorithms | every key and signature carries an algorithm tag | in keys |

The **protocol version** `proto_ver` (a `u32`, `major × 100 + minor`) names the whole set of
mandatory behaviours of a release generation. v2.0 is **200**. It appears in the handshake payloads
([handshake.md](handshake.md) §7) and in the node card. `MinProtocolVersion` is 200: a v2 node MUST
reject a peer with `proto_ver < 200` (REJECT `version`, `min = 200`, `max = 200`).

`proto_ver` minor increments add optional behaviour only (new features, new services, new object
labels). A change that alters a mandatory behaviour is a new major version and gets a new L1 suite
name (`EchoFog/3/…`).

## 2. Rules common to all layers

1. **Offer a list, choose one, bind the choice to the transcript.** Whatever is negotiated —
   handshake suite, features, services — both complete lists go into the handshake hash. A downgrade
   by an active attacker therefore breaks the handshake (as the protocol name does in Noise and the
   downgrade sentinel does in TLS 1.3).
2. **Unknown is not punished.**
   - Unknown service → `UNSUPPORTED{service, min, max}` and the stream closes; the session stays.
   - Unknown frame type with IGNORABLE → skipped.
   - Unknown frame type without IGNORABLE → `CLOSE{unknown-frame}` of that stream only.
   - Unknown protobuf field → skipped (and preserved in signed bodies).
   - Unknown feature, suite, algorithm tag, DHT record kind → ignored, or stored under quota (DHT).
   - None of these may raise an abuse score. Abuse scoring applies only to frames that are
     malformed for a *known* type.
3. **Refusals are explained** inside the encrypted channel before closing:
   `REJECT{reason, min, max, detail, retry_after}` ([registries.md](registries.md) §10.1). The only
   exception is a hidden node before a valid access proof, which stays silent.
4. **Version lifetime is published in the signed release manifest** ([objects.md](objects.md) §9),
   signed by the release key, not by any node key: `min_supported`, `recommended` and
   `deprecated_after` per component. A client shows "please update" when its version is below
   `recommended` or past `deprecated_after`; a node refuses peers below `min_supported` with
   REJECT `version`.
5. **Dual stack is a mechanism, not a v2 task.** A node MAY implement the two most recent major
   versions of an L1 suite or an L3 service at once (§4). v2 implements one: there is no v1 to keep.
6. **No version on the L0 wire.** A new masking scheme is a new suite ID; a node advertises the suites
   it accepts in its card; an old suite is switched off by removing it from the card and letting the
   last card that listed it expire (at most 7 days).
7. **Crypto agility through tags.** Each key and signature carries its algorithm tag
   ([registries.md](registries.md) §8). Moving to post-quantum signatures means adding `mldsa65` or
   the hybrid tag and accepting it, not changing any format.
8. **Every rule has test vectors and a CI check.** Each deterministic construction has vectors in
   [vectors/](vectors/); from v3 on, CI runs a node of the current version against a node of the
   previous one.

## 3. Per-layer details

### 3.1 L0

- A responder accepts an L0 suite exactly when one of its live cards lists an address with that
  suite. Recognition is by trial: noise3 by the selector MAC, REALITY v2 by the token
  ([wire.md](wire.md) §2.5, §3.3). There is no L0 version byte and none may be added.
- Suite parameters (SNI, shape profile, WebSocket path) are data in the card (`Addr.params`) and can
  change with every card.
- A masking key has `not_after`. The responder keeps accepting a key until `not_after` even after it
  publishes a card without it; this is how keys rotate.

### 3.2 L1

- Suite IDs and names: [registries.md](registries.md) §6.2. The full protocol name is the Noise
  `protocol_name`, so it is the first input to the handshake hash.
- The initiator chooses the suite before sending the first message (Noise requires it) from the
  intersection of its own list and the suites in the responder's card, and sends its full offered
  list in `HS_HELLO`. The responder MUST abort if the chosen suite is not its most-preferred suite in
  that intersection, and answers `REJECT{suite}` (public nodes only) listing its suites, so a stale
  card costs one retry, not a failure ([handshake.md](handshake.md) §3).
- `HS_HELLO` is part of the prologue: a man in the middle who edits the offered list makes the
  handshake fail.

### 3.3 L2

- Features are listed in `HsInit.features` and `HsResp.features`. The responder's list is the
  selected subset (the intersection with the initiator's list, GREASE excluded). The initiator MUST
  abort if the responder selected a feature it did not offer.
- A feature never changes the meaning of an existing frame; it adds frames or permits behaviour. A
  frame introduced by a feature is sent only after the feature is negotiated.

### 3.4 L3

- `HsInit.services` lists `(service, versions[])` the initiator can use; `HsResp.services` lists, per
  service, the single version the responder selects (the highest common). An empty selection means
  the responder does not offer that service to this peer.
- Each stream opens with `OPEN{service, version}`. Opening a pair that was not selected is answered
  with `UNSUPPORTED`. A service version is independent of `proto_ver`: `call/3` can ship in a 2.x
  release.
- A service version bump is required when an existing service frame changes meaning or a mandatory
  rule changes. Adding a frame type that the peer may ignore, or a protobuf field, is not a bump.

### 3.5 L4 — protobuf evolution

1. Field numbers are never reused. A removed field is listed as `reserved` in the schema.
2. A new field MUST have a default whose meaning equals "absent"; readers that do not know it
   behave correctly.
3. Readers MUST skip unknown fields. Relays and stores MUST keep unknown fields of signed bodies
   byte-for-byte (they never re-encode).
4. `oneof` alternatives are added, never repurposed; a reader that sees an unknown alternative treats
   the message as "unknown kind" (ignored or `UNSUPPORTED`, per the service).
5. Enums: unknown values are preserved and treated as the documented "unknown" behaviour, which every
   enum defines.
6. `map` fields are forbidden (no canonical order). `float`/`double` are forbidden in signed bodies.
7. Field numbers 1–15 are for frequent fields; 15 is conventionally `pad` (random padding) where a
   message has one.

### 3.6 L5 — signed objects

- The label carries the object version (`echofog/card/2`). A new incompatible object format gets a
  new label (`echofog/card/3`); both may coexist in the DHT, each under its own key.
- The envelope carries `seq` and `not_after` outside the body, so stores can order and expire objects
  of labels they do not understand ([objects.md](objects.md) §1).
- The signature covers the exact bytes. No implementation ever re-serialises a received object
  (K3: the v1 exit advert broke on this).

### 3.7 L6 — storage

- Every local database and key file starts with a schema number. A v2 node refuses to open a store
  with a higher number and migrates a lower one forward after writing a backup copy.
- v2 stores begin at schema 200. Nothing of v1 is migrated except the mnemonic-derived identity and
  user data (contacts, messages), through an explicit import.

### 3.8 L7 — algorithms

- `akey` and signature formats: [objects.md](objects.md) §1, tags in
  [registries.md](registries.md) §8.
- An object signed with an unknown algorithm is unusable but not malformed.
- Migration to post-quantum signatures: a node publishes a card signed with `ed25519+mldsa65`
  (hybrid). Because node IDs hash the tagged key, the hybrid identity has a new node ID; the old
  identity vouches for the new one with a device list (`echofog/devices/2`) signed by both. The
  details are left to the version that introduces the algorithm.

## 4. Dual stack for future major versions

When v3 appears, a node MAY support `{EchoFog/2, EchoFog/3}` suites at once:

- its card lists both L1 suites; the initiator picks the best common one; the responder enforces the
  best common one (rule 3.2), so neither side can be pushed down to v2 when both speak v3;
- the window is the **two most recent major versions**; the release manifest's `min_supported`
  closes the older one on a published date;
- services are versioned independently, so a v3 session may still use `chat/2` if that is the best
  common version;
- signed objects: a v3 node publishes both `card/2` and `card/3` until `min_supported` passes v2.

## 5. The release manifest

The release manifest ([objects.md](objects.md) §9) is a signed object with label
`echofog/release/2`, signed by a **release key** whose public part is compiled into every build
(a list of up to 3 keys; any one valid signature suffices). It is distributed through the DHT
(kind `echofog/release/2`), PEX responses from servers, and the project's download page.

For each component it gives:

| Field | Meaning |
|---|---|
| `component` | `proto`, `l1:<suite id>`, `svc:<service id>`, `obj:<label>`, `l0:<suite id>` |
| `min_supported` | below this, peers are refused (`REJECT version`) |
| `recommended` | below this, the user is told to update |
| `deprecated_after` | unix time after which the component is treated as below `min_supported` |

Rules:

- A node applies a manifest only if its signature verifies with a compiled-in release key, its `seq`
  is higher than the last applied one, and its `not_after` has not passed.
- A manifest MUST NOT raise `min_supported` above the version of the build that is reading it (a
  node never locks itself out); such an entry is shown to the user as "update required" and ignored
  for refusals.
- For v2.0: `proto` `min_supported = 200`, `recommended = 200`.
