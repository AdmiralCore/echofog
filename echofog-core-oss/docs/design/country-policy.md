# Country policy (AD18, T6.4)

Local policy of the first hop, the carrier and the rotation, by the client's country and the kind of
its network. Nothing here is on the wire: no frame, object or vector depends on it. Code:
`internal/v2/routepolicy` (the table and the rules file), `internal/v2/node/route_policy.go` (use).

## Inputs

| Input | Source |
|---|---|
| own country | GeoIP (`Host.Country`) of the first public address of the client (`local_addrs`, T3.4) |
| destination class | `domestic` (same country), `neighbor` (listed in the rules file), `foreign` (all else; unknown counts as foreign) |
| network class | `mobile` or `wired`; a user setting on the desktop, `TRANSPORT_CELLULAR` on Android; unknown counts as wired (`Config.NetClass`, `Runtime.SetNetClass`) |

A `neighbor` decides exactly like `foreign` until the field measurement (T15.2a) says otherwise. The
class exists so that the data and the connection report can name it.

## Default table (worst case, until measured)

| Client | Destination | Carriers | TCP volume | UDP | Rotation (H7) | First hop |
|---|---|---|---|---|---|---|
| RU mobile | foreign / neighbour | none; a probe of at most 12288 bytes over TCP | 12288 | no | no | entry node in RU, inside a verified `allowed_cidrs` range |
| RU mobile | own country | noise3, reality2-quic, reality2-tcp, ws2 | unlimited | yes | no | — |
| RU wired or unknown | foreign | all (D21 leaves the camouflaged ones) | unlimited | yes | yes | — |
| CN | foreign | reality2-quic, reality2-tcp, ws2 | unlimited | yes | yes | — |
| IR | foreign | reality2-tcp, ws2 (QUIC only when `udp` is allowed) | unlimited | no | yes | — |
| others | foreign | all | unlimited | yes | yes | — |
| any | own country | all | unlimited | yes | no | — |

If the first hop is required and the rules verify no range at all, the answer is `ErrRouteUnsupported`
("v2.0 does not support this network"); with ranges but no entry inside them it is
`ErrNoDomesticEntry`. Both come with the event `node.no-domestic-entry` (a clear message in Russian
for the application), never a quiet slow path (H14).

A **probe** (`DialOpt.Probe`) is the only direct connection a forbidden row allows: TCP suites only,
and the socket is closed after the row's probe budget in bytes (both directions).

## The rules file

Signed with a separate rules key (the owner's, offline): the file is the 64-byte Ed25519 signature
of `"echofog/v2/country-rules/1\0" || body`, followed by the JSON body. It is not a card, not an
object and not on the wire.

```json
{
  "seq": 7,
  "version": "2026-11-06",
  "allowed_cidrs": ["198.51.100.0/24"],
  "mobile_asn": [8359],
  "neighbors": {"RU": ["KZ", "BY"]},
  "tcp_limit": 8192,
  "udp_denied": ["IR"]
}
```

* `seq` is monotonic: the application passes the highest `seq` it has seen to `routepolicy.Parse`
  and refuses an older file. An expired file changes nothing: the file can only tighten the table,
  so the table never gets weaker with time.
* The file can only tighten. A `tcp_limit` lowers (or adds) the volume of rows that have a TCP
  limit towards abroad and never raises it; `udp_denied` switches UDP off; `neighbors` and
  `mobile_asn` are data only; `allowed_cidrs` names the entries that a curator has checked from a
  real SIM (H14) — an entry outside them is not used for RU mobile. Unknown fields are ignored.
* Size at most 64 KiB, at most 256 ranges, 1024 ASNs, 64 neighbour entries.

## What is not done yet

* Rotation by volume and time (H7) is a field of the decision (`Rotate`); the exit and the large
  transfers do not yet read it.
* The volume limit of a direct row is enforced for probes only; freeze detection (a stream stopped
  at 12–20 KiB marks the network as mobile) and the cache by network ID are later.
* Bootstrap dials (no card) are not filtered.
* The connection report (T11.2) can read `Runtime.RouteReport(peer)`: the rule, the table version and
  whether the first hop was an in-country entry.
