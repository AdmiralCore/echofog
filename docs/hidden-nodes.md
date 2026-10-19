Русская версия: [hidden-nodes.ru.md](hidden-nodes.ru.md)

# Hidden and isolated nodes

By default an EchoFog node is **public**: it is in the network's address book, and when it shares
its connection, the exit is advertised so anybody can find it and use it at its price. Sometimes
you want the opposite: an exit for your family, a team or paying subscribers, which strangers and
scanners cannot find at all. That is what the hidden and isolated modes are for.

> **Status: experimental**, like the rest of EchoFog. See [Run a node](run-a-node.md).

## The three modes

| Mode | Advertised in the exit market | Address handed on to other peers | Connects to public nodes | Who can use it |
|---|---|---|---|---|
| **Public** (default) | yes | yes | yes | everybody |
| **Hidden** | no | no | yes, as a client only: to find peers, use relays, punch through NAT | people with its link |
| **Isolated** | no | no | no | people with its link, connecting directly |

- **Hidden** suits a home computer or a VPS that still wants the network for itself: calls,
  messages, its own VPN. It relays for nobody, keeps no DHT records for others and does not
  announce its port, so the peers it talks to have no address of it to pass on.
- **Isolated** suits a VPS with a public address that only serves the people it invited. It dials
  nobody, not even the bootstrap nodes, and simply waits for connections. It needs an address
  people can reach: a public IP, or a port forwarded to it.

## The invitation link

A hidden or isolated node has an **access secret**. Its link carries the secret together with
where the node is:

```
echofog://join?v=1&net=echofog&p=101&id=<peer id>&s=<secret>&a=<host:port>&r=<relay>
```

| Part | Meaning |
|---|---|
| `v` | format of the link. An app that is too old to read it says "update the app" instead of failing silently. |
| `net` | the network the link belongs to: `echofog`, or a fork's own name. The app says "this link is for another network" instead of trying to connect. A fork sets its name at build time: `-ldflags "-X github.com/echofog/echofog-core/internal/config.NetworkName=myfork"`. |
| `p` | protocol version the node speaks |
| `id` | the node's peer ID |
| `s` | the access secret (only in the link of a hidden or isolated node) |
| `a` | an address to dial (may repeat) |
| `r` | a relay the node is connected to, for when no address can be dialed: the app connects to the relay and punches through NAT from there (hidden nodes only; may repeat) |

A public node's link has no `s`: it only tells where the node is.

### What the secret protects

- A peer proves it knows the secret **inside the handshake**, before the node says a single word:
  an HMAC over the connection's TLS channel binding, so a proof recorded on one connection is
  worthless on another. Whoever cannot prove it gets nothing, not even the node's public key.
- A hole punch request carries a short-lived ticket made from the secret, so a hidden node behind
  NAT does not reveal its addresses to strangers who ask through a relay.
- The exit itself checks again: it serves only the peers it let in, even over connections the
  hidden node opened itself (to relays and DHT nodes). To anybody else it looks switched off.

**Your contacts** reach a hidden node without the link, so calls and messages keep working.

### Who joined, and a new link

Whoever came in with the link is remembered (in the encrypted part of the node's database). A
**new link** replaces the secret: the old link lets nobody new in, but those who already joined
stay. To cut somebody off, remove them from the list: they then need the current link to come back.

The link gives **access, not free traffic**: people pay the price you set, like everybody else. To
let somebody use your exit for free, put them on the free list (`--exit-free-peers`, or
**Proxy → Free for these people** in the app).

## In the desktop app

**Proxy → Access to this computer**:

- **Public / Hidden** switches the mode. It takes effect after you restart the app.
- **Your invitation link** (or **Your address** in public mode) is the link to give out, with a copy
  button and a QR code.
- **New link** makes a new secret; **Joined with the link** lists who came in, with a button to
  remove each one.

To use somebody's link: **Proxy → Specific node**, paste the link, **Connect**.

Isolated mode is for servers and is not offered in the app.

## On a server

```bash
echofog-node run --access hidden --exit --exit-price 0.05
```

```bash
echofog-node run --access isolated --exit --exit-price 0.05 --public-addr 203.0.113.5:443
```

| Flag | Environment | What it does |
|---|---|---|
| `--access` | `ECHOFOG_ACCESS` | `public` (default), `hidden` or `isolated` |
| `--access-secret` | `ECHOFOG_ACCESS_SECRET` | the access secret, 22 characters of base64url. Empty: one is generated on the first start and kept in the data directory. Give several nodes the same secret to let one link in everywhere, or keep it in a secret store (a Docker or Kubernetes secret). A secret given here cannot be rotated with `invite --rotate`: change it here. |
| `--public-addr` | `ECHOFOG_PUBLIC_ADDR` | addresses put into the link (`host:port` or a `ws://` URL, comma-separated), when the observed one is not what people should dial: a domain, a port map |

A hidden or isolated node does not need `--exit-upload-mbps`: it advertises no speed.

The link and the list of who joined come from the running node:

```bash
echofog-node invite             # print the link
echofog-node invite --list      # who joined with it
echofog-node invite --rotate    # new secret: the old link lets nobody new in
echofog-node invite --remove <peer id>
```

These go through the local status endpoint (`--status-addr`, loopback by default), so they work
only on the machine the node runs on.

To use a hidden exit from another server: `echofog-node run --use-exit '<link>'` (the quotes keep
the shell from reading `&`).

## Limits

- Old versions of the app do not know these modes: they cannot read invitation links, and a hidden
  node does not let them in.
- Without obfuscation (`--obfs off`), the QUIC TLS certificate carries the node's public key, so a
  scanner who completes a TLS handshake learns the peer ID of the node at that address, though it
  still gets no further. Keep obfuscation on (the default).
- A hidden node behind NAT is reachable only through the relays named in its link. When it moves
  to other relays, give out a fresh link.
- Changing the mode takes a restart.
- Like every node, a hidden node answers peer-exchange and DHT queries from the peers it is
  connected to, with the public addresses of the peers it knows; among them may be the people who
  use it. Its own address is never in those answers.
