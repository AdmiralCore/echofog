# Protecting a node

What a public node (a relay, a signaling node or an exit on a VPS) is protected against, how to set
it up, how to see the protection at work, and what it does not cover. Every flag and its default
is listed in [Run a node](run-a-node.md); this page explains why they are there.

## The problem in one sentence

A peer ID costs nothing to make (it is the hash of a key anyone can generate), so a node can never
assume that one ID is one person. Every limit that counts per peer is backed by a limit per address,
per subnet and for the whole node.

## What a node is protected against

| Attack | What stops it |
|---|---|
| **Services a server does not need.** Chat, calls and file transfer accept data from strangers and write to disk. | `--role server` turns them off. Their messages are dropped by type on arrival, before any signature check, decryption or disk write. The node hands out no keys for chats and publishes none. |
| **Connection floods.** Many connections, or many identities from one machine. | At most 256 incoming peers (2048 on a server), 16 per address, 64 per /24 (IPv6 /56); at most 32 different IDs from one address in 10 minutes. When full, known peers take the place of the youngest strangers; long-lived peers are never pushed out. |
| **Handshake floods with forged addresses.** | A server (and any node under a flood) makes a new address answer a QUIC Retry first, so a forged address costs no TLS work. A server asks strangers for a small proof of work; under a flood every node asks everyone for a bigger one. Failed handshakes get an address refused for 15 minutes. WebSocket connections go through the same checks. |
| **Misbehaving peers.** Bad signatures, broken frames, floods hitting limits. | Points that halve every 10 minutes; 100 points ban the peer for an hour, 200 among the identities of one address ban the address for 15 minutes. |
| **One peer crowding out the rest.** | Each peer gets at most an eighth of the incoming message queue, 1024 streams and 16 file sessions. |
| **Amplification.** A small request that makes the node send a lot. | Speed probes (12 MB each): one per peer every 5 s, two at once, 48 MB a minute for the whole node, or off. Peer lists: answered only to the connected peer that asks, at most 16 entries, rate-limited; lists are taken in only when the node asked for them, and only public addresses. |
| **Filling the disk or memory.** | Offline mail: quotas per sender, per recipient and in total, with expired mail purged every 10 minutes. DHT records: deleted when they expire, at most 10 000 held, 5000 new an hour. Keys handed out on request: unused ones go after 72 hours, at most 2048. Known peers: at most 2000 that were only heard of. Every per-peer table is bounded. |
| **Taking over the relay.** Holding every slot, or pushing unlimited traffic through it. | Circuits per peer, per address and in total, with a quarter of the slots kept for peers connected 10 minutes or more. A circuit moving less than 32 KB in 5 minutes is closed. Byte budgets per circuit and per peer a day, a speed limit per peer, and optionally for the whole node. |
| **Draining the exit.** Too many connections, held open forever, or one user taking the whole uplink of a free exit. | Streams and UDP sessions per peer, per address and in total; an idle TCP connection is closed after 5 minutes; on a free exit, a fair share of speed per peer and per address, and optional traffic caps per peer and per address a day and for the whole exit a month. Paying clients and your own free list are never slowed down. |
| **Fake payments keeping the node busy.** | A voucher from an unknown vault is checked on the TON network at most every 10 seconds per peer, and an empty vault is remembered for 30 seconds. |
| **Being probed and recognised as EchoFog.** | The anti-DPI layer: see [Anti-DPI](anti-dpi.md). A stranger without the node's secret gets silence on UDP and the real cover site on TCP. |
| **Being found at all.** | Hidden and isolated nodes are not in the DHT and answer only people with an invitation: see [Hidden and isolated nodes](hidden-nodes.md). |

## Setting up a VPS

The Docker setup in `deploy/` already uses the server role. On top of the defaults:

1. **Keep `ECHOFOG_ROLE=server`.** Nobody can chat with the node directly; mail for other people
   passing through the relay still works.
2. **Set `ECHOFOG_RELAY_MBPS` below your uplink**, so relayed traffic cannot take the bandwidth
   the exit or the node itself needs. It is unlimited by default because the node does not know
   your plan.
3. **Open only the node's port** (443 by default, UDP and TCP) in the provider's firewall. The status
   endpoint listens on 127.0.0.1 only.
4. **Behind a CDN** (WebSocket through Cloudflare), set
   `ECHOFOG_WS_CLIENT_IP_HEADER=CF-Connecting-IP`, but only when the WebSocket port can be reached
   through the CDN alone: anyone who reaches it directly can write any address into that header.
5. **For people you know**, use a hidden exit with invitation links rather than a public one.

## Seeing it work

- `echofog-node status` shows the role, the peers, relay traffic with how often its limits were hit
  (`limits: … opens refused, … frames dropped, … closed over budget`), and exit refusals by reason
  (`too many streams`, `too many requests`).
- The log says `banning a misbehaving peer` or `banning an address` when it bans, and sums up dropped
  messages in one `Dispatcher: dropping messages` line at most every 10 seconds. Repeated events are
  folded into one line with a count, so a flood does not fill the disk with log.

## When the limits are too tight

- **Many people behind one address** (a university, a mobile operator's CGNAT) can hit the
  per-address limits: raise `--max-peers-per-ip` and `--max-ids-per-ip`.
- **A test cluster or a LAN** is not affected: the node's own machine is never banned, and private
  addresses have no subnet limits.
- **Every limit can be changed or turned off** with its flag (`-1` removes it, `0` keeps the
  default); see the tables in [Run a node](run-a-node.md).

## What this does not protect against

- **A flood that fills the uplink.** If more traffic arrives than the server's connection can carry,
  nothing the node does helps: that needs the provider's DDoS protection.
- **An attacker with many real addresses** (a botnet). Per-address limits slow them down; they do
  not stop them.
- **What exit users do on the Internet.** The exit blocks private networks and a few abuse ports;
  the rest is the operator's legal question (see the [FAQ](faq.md)).
- **A censor who lists public nodes.** Public nodes are in the DHT by design; hidden nodes are the
  answer.

The defaults come from tests on one machine and in Docker, not yet from real nodes under real load.
They may change as reports come in.
