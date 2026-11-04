# Operator clusters (`cluster/2`)

Status: draft, part of the v2 specification. Reserved names and numbers are in
[registries.md](registries.md); this document gives the formats and rules. Nothing here changes L0–L2
or any other service: a node that does not know `cluster/2` is a normal node and ignores the card
field and flag below.

An **operator cluster** is a group of nodes run by one operator: one or more **masters** and any
number of **workers** (exits, relays, mailboxes). To a client the cluster is a set of ordinary nodes
with ordinary cards; the cluster adds three things on top: a proof that the nodes belong together, a
control service between the operator's own nodes, and a pass that works on every node of the cluster.

Scope. The protocol defines membership, control, balancing hints, cluster passes and anonymous
reports. The operator panel, the Operator API, MCP access, sales and notification bots, payments,
payouts, referrals, showcase pages and imports from other panels are products on top of the Operator
API; they are not part of the wire protocol (see §10).

## 1. Keys and identity

```
cluster root key   C        ed25519, created once; may be kept offline
cluster_id         SHA-256("echofog/cluster/id" ‖ akey(C))[0:16]
master key         M_i      ed25519 (the master node's account key), delegated by C
worker             a node; identified by its account key (node ID, objects.md §3)
```

**Decision D42 (root key and delegated masters).** Workers do not trust a master's own key; they
trust the root key `C` and the masters it delegates to. Replacing or adding a master is therefore a
new signed delegation, not a change of every worker's configuration, and a standby master can take
over without any worker being re-enrolled. In the simplest case (one master, no standby) `C` is the
master's account key and no delegation is needed; a verifier treats a signer equal to `C` as
implicitly delegated.

### 1.1 Master delegation (`echofog/cluster-master/2`)

Signed by `C`.

```
message ClusterMaster {
  bytes  cluster_id = 1;          // 16 bytes
  repeated MasterEntry masters = 2;   // 1..4
}
message MasterEntry {
  bytes  key = 1;                 // akey (ed25519): the master's account key
  uint32 priority = 2;            // 0 is the preferred active master; distinct values
}
```

`not_after` ≤ 90 days; `seq` increases with every change. A delegation is valid for a verifier
when it is signed by `C`, its `cluster_id` matches `C`, and its `not_after` has not passed. A master
that renews nothing becomes untrusted when its delegation lapses, which bounds the damage of a stolen
master key: the operator signs a new delegation without that master, and workers learn it at their
next HEARTBEAT (§4.3) or ASSIGN.

### 1.2 Membership (`echofog/cluster/2`)

Signed by a delegated master (or by `C`).

```
message ClusterMembership {
  bytes  cluster_id = 1;          // 16 bytes
  bytes  member = 2;              // akey: the member's account key
  uint32 role = 3;                // 1 master, 2 worker; other values are treated as 2
  uint64 epoch = 4;               // cluster-wide change counter, §5
  uint32 caps = 5;                // bit0 exit, bit1 relay, bit2 mailbox, bit3 dht-server; informational
  string region = 6;              // ISO 3166-1 alpha-2, upper case; may differ from ExitOffer.region
  uint32 weight = 7;              // relative capacity for client-side balancing, 1..1000; 0 = 1
}
```

`not_after` ≤ 14 days. A member renews it through HEARTBEAT (§4.3) well before it expires. A card
that carries an expired membership is treated as a card without a cluster reference.

## 2. Card field and flag

`NodeCard` gets one optional field and one flag bit ([objects.md](objects.md) §3.1,
[registries.md](registries.md) §11):

```
ClusterRef cluster = 19;          // scope public and peer only; never in an invite card

message ClusterRef {
  bytes root = 1;                 // akey (ed25519) of C
  bytes delegation = 2;           // echofog/cluster-master/2 envelope; empty when the signer of
                                  //   `membership` is C itself
  bytes membership = 3;           // echofog/cluster/2 envelope for this node
}
```

Flag bit 7 `cluster` is set when the field is present. A verifier accepts the reference when:
(1) `membership.signer` is `C` or appears in a valid delegation; (2) `membership.member` equals the
card's `account`; (3) `cluster_id` in both objects equals the one derived from `root`; (4) neither
object has expired. Otherwise the field is ignored and the card is used as a normal card (a bad
reference never makes a card invalid: the node may be useful on its own).

A client shows nodes with the same `root` as one group. A reference proves membership only; it is not
a quality signal (§6). `ClusterRef` is about 600 bytes with a delegation and 300 without, within the
card limit.

## 3. Joining (self-discovery)

An operator enrols a worker with a **cluster link**:

```
echofog://cluster?v=2&root=<akey64>&master=<card64>&t=<token64>
```

Encodings follow [objects.md](objects.md) §10.1 (`card64` is the master's card, `akey64` and
`token64` are base64url without padding). `t` is a 16-byte one-time token minted by the master with an
expiry (default 24 hours, at most 7 days) and a use count of 1. The link is a secret: it lets one
node join, and is invalid after use or expiry. Cluster links are never shown to ordinary users and
are not accepted by the contact-link handlers.

Flow:

1. The new worker connects to the master as to any node (the master's card in the link gives
   everything needed for L0/L1) and opens `cluster/2`.
2. It sends `JOIN{token, caps, region, upload_kbps}`. The master checks the token, enrols the worker,
   signs a `ClusterMembership` and answers `JOINED{membership, delegation}`.
3. The worker puts both into its card (§2), republishes the card as usual, and starts the HEARTBEAT
   stream.
4. The master answers further `JOIN`s from the same worker's account key after the first without a
   token (the account key is already a member), so a worker can recover after losing its state only if
   the master still lists it.

Cluster members learn about each other from the master: `JOINED` and `ASSIGN` carry the signed
memberships of the other members the worker may use as standby paths. Workers never need to find the
master by DHT lookup: the master card in the link is enough, and later the master's card is
refreshed through `ASSIGN`.

## 4. The `cluster/2` service (service 11)

Opened by a worker towards a master. A master that is not the active master (§5) answers with
`NOT_ACTIVE{active}` (below). `cluster/2` is accepted only from members and for
`JOIN` from holders of a valid token; an unknown peer gets `UNSUPPORTED` as for any service the node
does not offer (no information about the cluster leaks). A session that carries `cluster/2` is not
sent through the diversity limits of a public service.

| Type | Frame | Body | Max |
|---|---|---|---|
| 0x40 | JOIN | `{ uint64 req = 1; bytes token = 2; uint32 caps = 3; string region = 4; uint32 upload_kbps = 5; }` | 96 |
| 0x41 | JOINED | `{ uint64 req = 1; bytes membership = 2; bytes delegation = 3; repeated bytes peers = 4; }` | 4096 |
| 0x42 | HEARTBEAT | `Heartbeat` (§4.3) | 512 |
| 0x43 | HEARTBEAT_ACK | `{ uint64 epoch = 1; bytes membership = 2; bytes delegation = 3; }` | 2048 |
| 0x44 | ASSIGN | `Assign` (§4.4) | 4096 |
| 0x45 | ASSIGNED | `{ uint64 req = 1; uint64 epoch = 2; uint32 code = 3; }` | 24 |
| 0x46 | REVOKE | `{ bytes member = 1; uint64 epoch = 2; }` | 64 |
| 0x47 | REPORT | `Report` (§8) | 2048 |
| 0x48 | SYNC | `Sync` (§5.2); master to standby master | 16384 |
| 0x49 | NOT_ACTIVE | `{ bytes active = 1; }` the active master's account key | 40 |
| 0x4a | ERROR | `{ uint64 req = 1; uint32 code = 2; }` codes in §4.5 | 16 |

Timeouts: JOIN 20 s to JOINED; ASSIGN 10 s to ASSIGNED.

### 4.3 HEARTBEAT

```
message Heartbeat {
  uint64 epoch = 1;               // the highest epoch the worker has seen
  uint32 clients = 2;             // sessions currently served
  uint32 load_pct = 3;            // 0..100, of the worker's own limit
  uint32 up_kbps_used = 4;        // measured, coarse
  uint32 flags = 5;               // bit0 cover site lost, bit1 UDP unavailable, bit2 low disk or memory,
                                  //   bit3 update available (security)
  string release = 6;             // build version, ≤ 32 bytes
  uint64 bytes_served = 7;        // since the last HEARTBEAT, for quota accounting (§7)
}
```

Sent every `uniform(25, 35)` s on a long-lived stream. The master answers HEARTBEAT_ACK; when the
worker's membership expires in less than 3 days, the ACK carries a renewed membership, and when the
delegation changed, the new delegation. A worker that gets no ACK for 5 minutes keeps serving on its
last policy (§4.4) and tries the next master by priority.

### 4.4 ASSIGN

The master's policy for one worker. All fields optional; a missing field keeps the current value.

```
message Assign {
  uint64 req = 1;
  uint64 epoch = 2;
  ExitOffer offer = 3;            // objects.md §4: price, free traffic, claimed capacity, policy
  repeated bytes pass_keys = 4;   // akeys that may sign cluster passes (§7); empty = masters only
  uint32 max_clients = 5;
  uint64 quota_slice_mib = 6;     // budget for pass holders until the next ASSIGN (§7)
  repeated bytes peers = 7;       // memberships of other members, for standby paths
  bytes  master_card = 8;         // the active master's current card
  repeated bytes revoked_passes = 9;  // pass_ids revoked since the previous ASSIGN (≤ 256)
}
```

A worker applies ASSIGN to its next card (new `seq`), within the card rules. The prices and limits the
worker publishes are the operator's own and are not secret.

### 4.5 Codes

| Code | Name |
|---|---|
| 0x100 | `bad-token` (unknown, used or expired) |
| 0x101 | `not-member` |
| 0x102 | `stale-epoch` |
| 0x103 | `delegation-invalid` |
| 0x104 | `too-many-members` (limit 256 per cluster) |

## 5. More than one master

Up to 4 masters are delegated (§1.1). **One is active at a time**: the live master with the lowest
`priority`. Workers keep the delegation and try masters in priority order; they follow the
`NOT_ACTIVE{active}` answer.

### 5.1 Epoch

Every change of membership, delegation or policy increments the cluster-wide `epoch`, which is stored
in `ClusterMembership.epoch` and `Assign.epoch`. The rule that prevents two active masters from
giving contradictory orders:

- A worker or standby accepts objects from any delegated master but acts on the one with the highest
  `epoch`; at an equal epoch the lower `priority` wins.
- A master that learns of a higher epoch signed by another delegated master (in a HEARTBEAT_ACK, a
  SYNC, or an ASSIGN) becomes standby until it again has the highest epoch.

### 5.2 Takeover

The active master sends `SYNC` (membership list, current policies, unused tokens, the pass keys, usage
counters) to every standby master every 60 s and on every change. A standby that has not received a
SYNC or a HEARTBEAT-bearing worker session from every higher-priority master for 3 minutes takes over:
it increments the epoch by 1, signs new memberships as needed, and announces itself with `ASSIGN` to
the workers (which accept it by §5.1). A returning higher-priority master sees the higher epoch,
pulls the SYNC state, and either waits as standby or takes over again by bumping the epoch (no more
often than once per 10 minutes, so a flapping master cannot keep the cluster busy).

Without a standby, a master outage changes nothing for clients: workers continue to serve under their
last policy until their memberships expire (14 days at most; HEARTBEAT renews them earlier), and
passes keep working until their own expiry (§7).

## 6. Balancing

The client chooses; there is no balancer in the path, so there is no single point of failure and
nothing for a censor to block together.

- A client groups cards by `ClusterRef.root`. For a group it picks a member by its own measured
  RTT and speed, weighted by `weight` (a member with weight 2 is chosen twice as often among equals),
  and by `ExitOffer.upload_kbps`.
- Cards carry only what they carry today: `ExitOffer.upload_kbps` is the operator's claim. Live load
  is **not** published in cards; a worker that is full refuses the connection (`RESULT` with the
  busy code of [registries.md](registries.md) §10.4) and the client tries the next member. This avoids handing a censor a cheap target list ranked by load.
- Recommendations to grow a cluster ("add a node in region X") are computed by the master from
  HEARTBEATs and REPORTs and shown in the panel; they are not wire data.

## 7. Cluster pass (`echofog/cluster-pass/2`)

A pass lets a client use any worker without paying again. Signed by the master or by a key listed in
`Assign.pass_keys` (so a sales bot can mint passes with a key that cannot administer the cluster).

```
message ClusterPass {
  bytes  cluster_id = 1;
  bytes  holder = 2;              // akey: the client's per-cluster identity (exit/2: per-exit rule applies per cluster)
  bytes  pass_id = 3;             // 8 random bytes; revocable
  uint32 scope = 4;               // 0 cluster (default), 1 listed nodes
  repeated bytes nodes = 5;       // scope 1: member account keys, ≤ 16
  uint64 quota_mib = 6;           // 0 = unlimited within the term
  uint32 caps = 7;                // bit0 exit, bit1 relay, bit2 mailbox: what the pass covers
}
```

`not_after` is the end of the term. **Decision D43 (pass scope).** The operator chooses the scope per
pass, and the default is the whole cluster: one pass works on every worker, so a client can move
between members without paying again. `scope = 1` restricts it to listed members (for example, a
premium region).

Use: the client sends the pass on an exit/2 stream in `PASS` (exit/2 type 0x4a, below) before
`TCP_CONNECT`. The worker verifies the signer (master, or a `pass_keys` key under a valid delegation),
the `cluster_id`, the expiry, that its own account key is allowed by the scope, that `holder` equals
the session's client identity, and that `pass_id` is not in its revoked list (`Assign.revoked_passes`).
It answers `PASS_RESULT` and then serves the holder like a paid account.

| exit/2 type | Frame | Body | Max |
|---|---|---|---|
| 0x4a | PASS | `{ uint64 req = 1; bytes pass = 2; }` the `echofog/cluster-pass/2` envelope | 1024 |
| 0x4b | PASS_RESULT | `{ uint64 req = 1; bool ok = 2; uint32 error = 3; uint64 quota_left_mib = 4; }` | 40 |

`error` uses the exit/2 RESULT codes plus 0x110 `pass-invalid`, 0x111 `pass-expired`, 0x112
`pass-wrong-node`, 0x113 `pass-revoked`, 0x114 `pass-exhausted`.

**Quota.** Each worker holds a budget slice for pass holders (`quota_slice_mib`) and reports
`bytes_served` in HEARTBEAT; the master adds up usage per `pass_id` and, when a pass is exhausted,
revokes it. A holder can overrun its quota by at most the sum of the slices of the workers it uses,
which the operator bounds by choosing small slices. Exact per-byte accounting across nodes is not
claimed.

## 8. Anonymous reports

**Decision D44 (reports are opt-in).** Anonymous reports from clients to the master are **off by
default** and sent only after the user switches them on. They exist so the operator can see which
regions or networks block a node and when to add capacity.

```
message Report {
  uint32 version = 1;             // 1
  uint64 period_start = 2;        // unix seconds, rounded down to a day
  string country = 3;             // ISO 3166-1 alpha-2, or empty
  uint32 net_class = 4;           // 0 unknown, 1 fixed, 2 mobile, 3 public wifi
  uint32 asn_hash = 5;            // 16 bits of SHA-256(asn ‖ period_start); lets the operator count
                                  //   distinct networks per day without knowing which
  repeated Counter counters = 6;  // ≤ 32
}
message Counter { uint32 kind = 1; uint32 value = 2; }   // kinds below
```

Counter kinds: 1 connect attempts, 2 connect successes, 3 handshake failures, 4 timeouts, 5 resets
during the first 16 KB, 6 UDP unavailable, 7 cover-site mismatch, 8 median setup time in ms. A report
contains no address, key, node or contact ID, no timestamp finer than a day, and no per-session
detail. It is sent in `cluster/2` `REPORT` over a fresh session that uses a one-time client identity
(not the per-exit identity), at a random time of day; the master cannot link two reports. The master
rejects reports with unknown counter kinds in bulk only, never with an error that identifies the
sender. Recipients ignore unknown fields.

## 9. Security notes

- **Forged membership.** Needs `C` or a delegated master key. Delegations expire in 90 days and
  memberships in 14, so a stolen master key stops working after the operator renews without it.
- **Hostile worker.** A worker can serve and measure traffic its own exit sees, but cannot issue
  passes (it lacks a pass key) or add members. A revoked worker (REVOKE) loses its membership at the
  next renewal and its pass checks fail as soon as `pass_keys` and the revoked list reach the others.
- **Cluster as a censorship target.** Cards of members share `root`, so a censor that finds one member
  can list the cluster's other published cards. Hidden workers (`unlisted`, scope peer or invite)
  carry the reference in their peer-scope cards only; operators who need cover SHOULD keep workers
  unlisted and give out only the cluster pass plus one hidden contact per client.
- **Operator identity.** `root` is a pseudonymous key; the protocol never ties it to a person.
- **Reports** are the only client-to-operator telemetry in the protocol and are opt-in.

## 10. What stays outside the protocol

The Operator API on the master (HTTP/gRPC on a local or tunnelled port; roles `read-only` and
`manage`; the key of an operator is not a cluster key), the web panel, the MCP server that exposes the
Operator API to an AI assistant, the Telegram sales and notification bots, e-mail and push
notifications, payment methods other than TON (the cards of exits only carry `shop` links,
[objects.md](objects.md) §3.5), payouts, referral statistics, public showcase pages, ratings and
badges, free-trial promotions, and import of clients from Marzban and 3x-ui. They use the same
signed passes (§7) and the same membership list as the protocol.

## 11. Order of work

1. Registries and card field (this specification).
2. `objects`: `ClusterMaster`, `ClusterMembership`, `ClusterPass` with vectors.
3. `cluster/2`: JOIN, HEARTBEAT, ASSIGN, REVOKE, SYNC with the epoch rules (a simulation test with a
   master failure).
4. Client grouping and weighted choice; pass presentation in `exit/2`.
5. Operator API and panel; then reports, bots, notifications.
