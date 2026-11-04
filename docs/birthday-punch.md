# Birthday hole punching (symmetric NAT)

Developer notes on how EchoFog reaches a peer behind a symmetric NAT without a relay. User-facing
text is in [reachability.md](reachability.md).

## The problem

Ordinary hole punching (DCUtR, `internal/p2p/holepunch`) needs each side's public address to stay
the same whoever it talks to. A **symmetric NAT** (most mobile/carrier networks) hands out a *new
external port for every destination*, so the port a relay saw is not the port a packet to the peer
will come from. The ordinary punch aims at the wrong port and fails, and the pair falls back to a
relay.

## The idea (birthday paradox)

Open many mappings on one side and probe many ports on the other; a pair collides with probability

```
P ≈ 1 − e^(−N·M / R)
```

- **N** — sockets the symmetric side opens (one external port each).
- **M** — packets the other side sprays across the range.
- **R** — width of the port range being searched.

The dominant lever is **R**. A carrier NAT that draws ports from a small block (a few thousand)
makes this near-certain with modest N, M; the whole dynamic range (~64k) needs N·M in the thousands
(N=256, M≈900 → P≈97%). HyperDHT/Holepunch and Tailscale use the same idea. The spray widens the
observed block by `birthdaySprayMargin` (4000 ports) on each side, because the observed range is
only a few STUN samples of the NAT's real block.

## Cases and roles (`chooseMode`)

Chosen from the pair of NAT types, which the two sides exchange in the CONNECT/ANSWER messages
(`NT`, `PR`, `PN` fields — backward-compatible: a v0.2.0 peer sends none and is treated as cone,
never sprayed to). The initiator chooses the mode from both reports and names the responder's half
in SYNC (`BD` plus the `BR` role); the responder runs that role instead of recomputing it, so the
two ends cannot act on different NAT pictures. A SYNC without `BR` (an older build) falls back to
the old recomputation.

| Our NAT | Peer NAT | Mode | Who does what |
|---|---|---|---|
| cone / public / mapped / unknown | cone / unknown / old | ordinary DCUtR | as before |
| **symmetric** | cone | `birthdayDial` | we open N sockets; peer sprays our range |
| cone | **symmetric** | `birthdaySpray` | we spray the peer's range; peer opens sockets |
| unknown | **symmetric** | `birthdaySpray` | best effort: if we turn out symmetric the attempt fails and the pair relays |
| **symmetric** | unknown / old | relay | the peer cannot spray back |
| **symmetric** | **symmetric** | `birthdayMutual` | both open sockets and spray; only when *both* ranges are narrow |
| anything with a wide symmetric range, or both symmetric and not both narrow | | relay | — |

The role names who does what, not who initiated. In the both-symmetric case the lower peer ID
dials and the higher listens (decided the same way on both sides, via `Config.OwnID`).

**Unknown NAT is advertised explicitly.** `ownNATInfo()` reports `Unknown: true` until the NAT
manager has classified the node (and in guard mode, where STUN is blocked). Advertising cone would
make a symmetric peer believe a spray can reach us; advertising unknown keeps the safe decision.

**Birthday is IPv4-only.** Its sockets and its spray target IPv4. When the negotiated pair has no
public IPv4 candidate (an IPv6-only network, or the peer's IPv4 was filtered), the mode falls back
to the ordinary punch over IPv6, which has no NAT to defeat.

## Transport (`internal/network/holepunch_birthday.go`)

- **`BirthdayDial`** (symmetric side): opens N fresh UDP sockets, sends a small noise packet from
  each toward the cone peer's fixed address (opening N NAT mappings), and watches all sockets. The
  first socket to receive a packet from the peer's IP dials the peer over QUIC **from that socket** —
  reusing the ephemeral-noise-dial path (`birthdayDialOn`: wrap with the node's obfuscation, a
  one-connection `quic.Transport`, authenticate, hand to the swarm). The rest are closed. The
  follow-up dial is noise-only TLS (no REALITY SNI, the noise ALPN): a birthday socket has no cover
  site to pretend to be.
- **`BirthdaySpray`** (cone side): sprays M noise packets from its listening socket across the
  peer's port block (`birthdayPorts`: a narrow block is swept, widened by 4000 ports on each side;
  a wide/unknown range goes random over 1024–65535), stopping the instant the peer's connection
  arrives. Before sending it calls `ExpectPeerIP`, because the peer is known by IP and every packet
  goes to a guessed port; without that key the packets would be sealed for the wrong node and the
  symmetric side would drop them.
- **`BirthdayMutual`** (both symmetric): each socket targets exactly **one** guessed port (a
  symmetric NAT gives a fresh external port per destination, so one destination per socket keeps
  that port stable). The dialer dials only a socket whose reply came from the exact port it
  targeted; the listener runs a QUIC listener per socket and keeps a socket alive for any accepted
  connection (`birthdayAccept`).

**One pacer for every role.** All three roles use `birthdayPaceLoop`, which sends one packet per
socket in round-robin at about `birthdayRate`. The schedule is absolute (packet *i* is due at
`start + i·interval`) rather than a timer reset after each send: a reset is measured from the wake-up,
so every late timer would push every later packet back, and over 900 packets Windows' timer latency
cut the budget by a fifth. With absolute deadlines a late wake is absorbed by a shorter next wait.
The jitter is small and one-sided down (down to 3% under the interval), so the average rate is the
target and the whole budget fits the window.

The punch packets are the same small random datagrams the ordinary punch sends, so they add **no new
wire fingerprint**. The QUIC dial that follows uses the node's normal obfuscation.

### Guards on the receiving side

A birthday socket is a public listener for a few seconds, so `birthdayAccept` treats it like the
main listener rather than a private door: it applies the handshake guard's per-IP admission and the
`handshakeSem` limit, authenticates with `WithExpectedPeer` so only the arranged peer is admitted,
and marks the resulting connection **not dialable** — its source port is a NAT mapping, not a
listening port. `BirthdayDial`/`BirthdaySpray`/`BirthdayMutual` reject non-unicast targets
(unspecified, multicast, link-local); loopback is allowed only when the transport itself is bound to
loopback, which keeps the tests and single-machine setups working without exposing a public node.

## Where it is triggered

`pkg/node/nat.go`: `ownNATInfo()` reports our NAT (`Sym` true only for a symmetric NAT with no
forwarded port, `Unknown` until classification, and neither when a forwarded port makes us reachable
the ordinary way). A cone node reaching a symmetric peer sprays in the foreground via
`connectToPeer`; a symmetric node reaching a cone peer dials in the background via `tryDirectLater`.
When a direct link comes up, calls and messages move to it on their own. A single 3 s attempt often
misses (the spray and the open mappings must overlap in time), so `Connect` makes the usual three
coordinated attempts; the background retrigger is spaced by `tryDirectLater`'s interval. The
symmetric side must already know it is symmetric when the attempt is made — classification takes a
couple of keep-alive cycles, so a punch very early in a node's life falls back to a relay until the
next try.

## It depends on knowing our NAT

Birthday only fires when the node has classified itself as **symmetric**. That classification
(`natcheck`) needs several independent observers of our *main* QUIC socket at distinct ports:

- **STUN** against several servers (the normal path), or
- **≥3 peers reached over noise on the main socket** (`ObserveFromPeers`).

A REALITY dial opens its own socket, so its observation is `IPOnly` and does **not** reveal the main
socket's mapping. So a node whose only public peers are camouflaged servers *and* whose STUN is
blocked cannot learn it is symmetric. That is exactly the **guard-mode** situation — and birthday is
meant to be off there anyway (see Limits): a burst of UDP across a censoring border is itself a
signal. Outside guard mode STUN settles the classification and birthday runs. So the feature serves
a symmetric user who is *not* behind an active censoring border (most mobile/carrier CGNAT); a user
in guard mode stays on a relay by design.

## Limits (anti-DPI)

A burst of UDP is itself a signal, so each attempt is bounded: ≤256 sockets (64 for mutual, where
both ends also listen), ≤900 spray packets, ≤3 s, paced to ~320 packets/s with small jitter; the
900-packet budget fits the window at that rate by design (900 / 320 ≈ 2.8 s). At most 2 birthday
punches run per node at once, and the background retrigger is spaced per peer (`tryDirectLater`).
Off in anonymous/Tor/proxy mode (the punch service is nil there) and across a censoring border (the
noise-dial rule filters those addresses, and guard mode blocks the STUN that would classify us).

## Tests

- Logic (`internal/p2p/holepunch/birthday_test.go`): the mode matrix (including unknown NAT and old
  peers), the role named in SYNC is authoritative on the responder, the IPv6 fallback to the
  ordinary punch, forbidden cases send no packets, the concurrency cap, and the transport
  interface signatures.
- Transport (`internal/network/holepunch_birthday_test.go`): port selection, the spray budget and
  pacing (the whole 900 packets inside 3 s, and the pacer reaching every socket), stop-on-arrival,
  a real obfuscated end-to-end punch, accept rejecting an unexpected peer and marking the session
  non-dialable, and dial/mutual timeout and dedupe.
- Obfuscation (`internal/network/dpi/obfs_test.go`): `ExpectIP` keys packets to guessed ports and
  keeps the peer's key in the receive candidates even when an IP has many keys.
- End-to-end (`testbed`, needs Docker): `go run ./testbed/runner -scenario birthday` sets `gw_b` to a
  narrow symmetric NAT and drives a cone→symmetric call (a call, not a message, triggers the
  direct-link attempt), checking the symmetric nodes end up with a direct link to the cone node and
  that the node counted a successful birthday (`/status` `birthdayTried`/`birthdayOK`), not just a
  direct link that might have come from another path. Verified passing on 2026-11-04: both
  `b1 → a1` and `b2 → c1` linked on round 1, with a successful birthday counted on each b node.
  Getting a reliable lab run needs a few things the field gives for free, so the scenario arranges
  them:
  - guard mode is stood down (birthday is for a user *not* behind a censoring border; guard mode
    blocks the STUN that classifies us and suppresses the punch by design);
  - the lab has no Internet STUN, so the server nodes answer STUN on `:3478` and every user node
    queries three of them — the same classification path the field uses;
  - it waits for *both* ends to finish NAT classification before calling, so the cone advertises its
    real external address (else the symmetric side punches a stale port and the spray is dropped);
  - the two nodes behind each gateway listen on different ports, so a cone gateway keeps a stable
    external port per node (a clean cone) instead of remapping it;
  - because one 3 s punch is probabilistic, it places the call in up to five rounds and passes on the
    first round that links — as a real multi-minute call would.
- Field: two machines on mobile operators; look for `Birthday punch reached a peer behind a
  symmetric NAT` in the log and a direct connection replacing the relay.
