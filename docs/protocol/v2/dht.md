# DHT v2

The DHT stores signed objects ([objects.md](objects.md) §2) and finds nodes. It is a Kademlia
variant hardened against the v1 weaknesses K4 (unsigned, overwritable node information; first answer
wins; replies not tied to the asked node) and V4 (amplification). The wire messages are those of the
`dht/2` service ([services.md](services.md) §2).

What it does and does not achieve is stated in [security.md](security.md) S27–S28: device IDs are cheap
to grind, so the DHT relies on network diversity, many answers and disjoint paths, and readers' memory
of what they have already seen. A censor with addresses in many networks can still hide records.

## 1. Keyspace

- 256-bit keys; distance is XOR.
- A node's position is its **device ID** `device_id = SHA-256(device akey)` ([objects.md](objects.md)
  §1); the account's node ID is not a DHT position. Positions are not
  chosen freely but are cheap to grind: about N/k key generations put a node among the k closest to a
  given key in a network of N nodes.
- `k = 20` (bucket size and result size), `α = 3` parallel requests per lookup step.
- **Network diversity unit**: an IPv4 /24 or an IPv6 /32 (IPv6 /48s are free from tunnel brokers and
  count as nothing). "Distinct networks" below always means distinct diversity units.

## 2. Record keys

A record is a signed object. Its DHT key is derived from its label, its signer and the current day, so
only the signer can publish under it and pre-ground positions go stale every day:

```
day            = floor(unix_time / 86400)
signer-keyed:  key = SHA-256("echofog/dht2/key" ‖ u32(day) ‖ u8(len(label)) ‖ label ‖ signer_id(32))
release:       key = SHA-256("echofog/dht2/release" ‖ u32(day))
```

- `signer_id` is `SHA-256` of the envelope signer's akey: the device ID for objects a device signs
  (cards, prekeys, mailbox hints), the node ID for the device list, which the account key signs. A
  contact who knows only a node ID first looks up the account's `devices/2` record, then the devices'
  cards and prekeys.
- Publishers store each record under the key of the current day and, during the last 2 hours of a day,
  also under the next day's key. Readers look up the current day's key and, during the first 2 hours of
  a day, also the previous day's.
- The release manifest has a fixed per-day key independent of the signer, so manifests signed by
  rotated release keys are found by every build ([objects.md](objects.md) §9).
- **Name-keyed records (`echofog/name/2`) are not stored in v2.** Without an on-chain ownership check,
  any signer could publish a name with a huge `seq` and block the real owner. The `names` service will
  define storage under `(name, signer)` with resolution against the chain. Until then, storing nodes
  refuse this label (`STORED{refused}`).

**Decision D9 (one record per label per signer).** v2 has no sub-keys: each signer has at most one
live record per label. Objects that need several values (several mailbox hints) carry them inside
one body. This keeps ownership checkable for labels a storing node does not understand.

Record kinds, sizes, lifetimes and replica counts: [registries.md](registries.md) §9.

## 3. Storing

`STORE{record}`. A storing node MUST:

1. verify the object ([objects.md](objects.md) §2.2), including its signature; for unknown labels
   starting with `echofog/` apply the generic limits and clip the lifetime to 24 hours; a `release/2` object is stored only if it also
   passes the node's release check (the compiled release keys and the threshold, [objects.md](objects.md) §9): its
   day key does not depend on the signer, so otherwise anybody could occupy it with a huge `seq`. A node without
   release keys stores none (`STORED{refused}`);
2. check that the request's key equals the derivation of §2 for the current or an adjacent day;
3. replace a stored record only with a higher `seq` from the same signer; equal `seq` with different
   bytes keeps the smaller `object_id`; a lower `seq` is refused (`STORED{stale}`);
4. drop records at `not_after` (or the clipped expiry);
5. enforce quotas:
   - per signer: at most 8 records of known labels and 4 of unknown labels;
   - per publishing session: 64 records, or 1024 if the publisher's card has `role = server` (guards
     publish for their clients); at most 300 `STORE` per hour and 2 per second;
   - per diversity unit of the publisher: 3 times the per-session values;
   - in total: 20 000 records. When full, the node evicts the stored record whose key is farthest from
     its own device ID, and refuses new records farther than that (`STORED{quota}`), so honest records
     near the node are never displaced by far-away spam.

A node stores only keys for which it is among the 20 closest nodes it knows, plus a cache of popular
records (at most 1000) that it serves but does not count as replicas.

Publishers store each record on the `replicas` closest nodes found by a lookup
([registries.md](registries.md) §9), chosen with **at most one node per diversity unit**, and
republish when half of the remaining lifetime has passed, or after a change, with ± 20 % jitter.

## 4. Lookup

### 4.1 Node lookup

Iterative: query the `α` closest known nodes with `FIND_NODE{target}`; each answers `NODES` with up to
20 **signed cards** ([objects.md](objects.md) §3); continue with the closest unqueried nodes until the
20 closest have answered or 64 queries have been sent. Per query timeout 2 s. The "20 closest" set is
formed with at most one node per diversity unit and at most 2 per /16 (IPv6 /24).

- Replies are bound to the asked node: they arrive on a `dht/2` stream of the authenticated session
  with that node and carry the request ID (K4: v1 matched replies by message ID only).
- Cards in `NODES` are verified before use; an invalid card is dropped and counted against the
  answering node.

### 4.2 Value lookup

`FIND_VALUE{key}` returns the stored record if present, and up to 20 closer cards otherwise.

- The lookup **does not stop at the first answers**: it continues until all of the 20 closest
  responsive nodes (formed with the diversity rule of §4.1) have answered, or 64 queries have been
  sent.
- It runs over **3 disjoint paths** (S/Kademlia style): three sets of queried nodes that share no node
  and no diversity unit, each path continuing only from nodes its own path returned. This is REQUIRED
  for `prekeys/2`, `card/2`, `mailbox-hint/2` and `release/2`, and RECOMMENDED for other labels.
- Result: the valid record with the highest `seq` seen on any path (ties: smaller `object_id`).
  "Not found" is concluded only when a majority of the answering closest nodes on every path said so.
- **Reader memory.** A reader keeps the highest `seq` it has ever accepted per (signer, label) and
  never accepts a lower one, so rollback can only affect a reader's first lookup of a record.
  Contacts' prekeys and cards also arrive inside sessions, which bypass the DHT.

### 4.3 Answering

A node answers `FIND_NODE` only with cards that are not `unlisted` and whose node it reached **at an
address of that card** in a working session within the last 24 hours. A card learned only from the
DHT or PEX, never connected to, is not passed on: otherwise anyone could sign cards that point at a
victim's address and make the network dial it (reflection, V4).

`NODES` is limited to 65536 bytes: if 20 cards do not fit, the farthest are dropped until they do.

## 5. Routing table

- 256 buckets of `k = 20`; entries are (device ID, the device's current public card).
- An entry is added or updated **only** from: an authenticated session with that node (the card from
  the handshake), or a valid card with a higher `seq` than the one held. A third party can never
  change the address of an entry: addresses come from the node's own card.
- Diversity: at most 2 entries per diversity unit per bucket, and at most 10 % of the table per /16
  (IPv6 /24).
- A full bucket replaces its least recently seen entry only after that entry fails a ping
  (`PING` on an existing session, or a new session); at most one such check per bucket at a time
  (V4: v1 spawned one dial per new ID).
- Nodes with `flags.unlisted` are never added, whatever the source; the responder of a `FIND_NODE`
  is added only from the card of its authenticated session.
- The table is saved every 5 minutes with jitter as the list of cards, so after a restart addresses and
  roles are known from signed data (C12).
- Refresh: each bucket not touched for `uniform(10, 15)` minutes is refreshed with a lookup of a random
  ID in it; a self-lookup runs at start-up.

## 6. Proxied lookups (guard mode)

A client in guard mode asks its guard servers to run lookups for it (as in v1). A server MUST limit
proxied lookups to 8 concurrent and 1 per second per client (burst 10), and each proxied lookup to 64
queries. Results are signed records and cards, which the client verifies itself. The guard learns
which records the client looks up ([security.md](security.md) §4).

## 7. What the DHT does not do

- No unsigned data. v1's `PeerInfo{address}` replies are gone.
- No enumeration of exits by the DHT. Exits are found through pex/2 `GET{exit}` from servers and
  peers ([services.md](services.md) §3), and their cards are verified.
- No private data: records of hidden or unlisted nodes are never published, and no noise3 address or
  key is ever in a public card.
