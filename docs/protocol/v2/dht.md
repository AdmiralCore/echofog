# DHT v2

The DHT stores signed objects ([objects.md](objects.md) §2) and finds nodes. It is a Kademlia
variant hardened against the v1 weaknesses K4 (unsigned, overwritable node information; first answer
wins; replies not tied to the asked node) and V4 (amplification). The wire messages are those of the
`dht/2` service ([services.md](services.md) §2).

## 1. Keyspace

- 256-bit keys; distance is XOR.
- A node's position is its `node_id = SHA-256(akey)` ([objects.md](objects.md) §1). Positions are
  therefore not chosen freely, but they are cheap to grind; the defences below do not depend on
  positions being random.
- `k = 20` (bucket size and result size), `α = 3` parallel requests per lookup step.

## 2. Record keys

A record is a signed object. Its DHT key is derived from its label and signer, so only the signer can
publish under it:

```
signer-keyed:  key = SHA-256("echofog/dht2/key" ‖ u8(len(label)) ‖ label ‖ signer_node_id(32))
name-keyed:    key = SHA-256("echofog/dht2/name-key" ‖ u8(len(n)) ‖ n)          n = normalised name
```

**Decision D9 (one record per label per signer).** v2 has no sub-keys: each signer has at most one
live record per label. Objects that need several values (several mailbox hints) carry them inside
one body. This keeps ownership checkable for labels a storing node does not understand.

Record kinds, sizes, lifetimes and replica counts: [registries.md](registries.md) §9.

## 3. Storing

`STORE{record}` (and `STORE{record, name}` for name-keyed kinds). A storing node MUST:

1. verify the object ([objects.md](objects.md) §2.2), including its signature; for unknown labels
   starting with `echofog/` apply the generic limits;
2. check that the request's key equals the derivation of §2 (name-keyed records: only for labels the
   node knows; an unknown label is always signer-keyed);
3. replace a stored record only with a higher `seq`; equal `seq` with different bytes keeps the
   smaller `object_id`; lower `seq` is refused (`STORED{status = stale}`) — rollback by replay of old
   records is impossible;
4. drop records at `not_after`;
5. enforce quotas: at most 64 records per publishing node, 16 of them of unknown labels; at most
   20 000 records in total; at most 300 `STORE` per publishing node per hour and 2 per second; per
   source /24 (IPv6 /48) 3 times the per-node values. The publishing node is the session peer that
   sent `STORE`, not the signer.

A node stores only keys for which it is among the 20 closest nodes it knows, plus a cache of popular
records (at most 1000) that it serves but does not count as replicas.

Publishers store each record on the `replicas` closest nodes found by a lookup
([registries.md](registries.md) §9) and republish when half of the remaining lifetime has passed, or
after a change, with `± 20 %` jitter.

## 4. Lookup

### 4.1 Node lookup

Iterative: query the `α` closest known nodes with `FIND_NODE{target}`; each answers `NODES` with up to
20 **signed cards** ([objects.md](objects.md) §3); continue with the closest unqueried nodes until the
20 closest have answered or 64 queries have been sent. Per query timeout 2 s.

- Replies are bound to the asked node: they arrive on a `dht/2` stream of the authenticated session
  with that node and carry the request ID (K4: v1 matched replies by message ID only).
- Cards in `NODES` are verified before use; an invalid card is dropped and counted against the
  answering node.
- Lookups for sensitive targets (prekeys, mailbox hints) SHOULD use 2 disjoint paths (S/Kademlia
  style): two sets of queried nodes that share no node, each lookup continuing only from nodes its own
  path returned.

### 4.2 Value lookup

`FIND_VALUE{key}` returns the stored record if present, and up to 20 closer cards otherwise.

**The lookup MUST NOT stop at the first record.** It continues until at least **3** valid records
(or 3 "not found" answers from the 8 closest nodes) have been collected from nodes in 3 different /24
(IPv6 /48) networks, then returns the record with the highest `seq` (ties: smaller `object_id`).

### 4.3 Answering

A node answers `FIND_NODE` only with cards it has verified and that are not `unlisted`, and that it
has seen in a working session in the last 24 hours or received from the DHT within their lifetime. A
node never answers with addresses it has not seen in a card.

## 5. Routing table

- 256 buckets of `k = 20`; entries are (node ID, the node's current card).
- An entry is added or updated **only** from: an authenticated session with that node (the card from
  the handshake), or a valid card with a higher `seq` than the one held. A third party can never
  change the address of an entry: addresses come from the node's own card.
- Diversity: at most 2 entries per /24 (IPv6 /48) per bucket, and at most 10 % of the table per /16.
- A full bucket replaces its least recently seen entry only after that entry fails a ping
  (`PING` on an existing session, or a new session); at most one such check per bucket at a time
  (V4: v1 spawned one dial per new ID).
- Nodes with `flags.unlisted` are never added, whatever the source; the responder of a `FIND_NODE`
  is added only from the card of its authenticated session.
- The table is saved every 5 minutes with jitter as the list of cards, so after a restart addresses and
  roles are known from signed data (S12).
- Refresh: each bucket not touched for `uniform(10, 15)` minutes is refreshed with a lookup of a random
  ID in it; a self-lookup runs at start-up.

## 6. Proxied lookups (guard mode)

A client in guard mode asks its guard servers to run lookups for it (as in v1). A server MUST limit
proxied lookups to 8 concurrent and 1 per second per client (burst 10), and each proxied lookup to 64
queries. Results are signed records and cards, which the client verifies itself.

## 7. What the DHT does not do

- No unsigned data. v1's `PeerInfo{address}` replies are gone.
- No enumeration of exits by the DHT. Exits are found through pex/2 `GET{exit}` from servers and
  peers ([services.md](services.md) §3), and their cards are verified.
- No private data: records of hidden or unlisted nodes are never published.
