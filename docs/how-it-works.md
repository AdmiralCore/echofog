Русская версия: [how-it-works.ru.md](how-it-works.ru.md)

# How EchoFog works

A plain-language tour for curious users and people who want to contribute. Each section is short
and links to a deeper document where one exists.

> **Status: experimental.** EchoFog has had no independent security audit by a specialist
> organization yet and has not been tested against a real national DPI. Do not rely on it in a
> high-risk situation yet. See [SECURITY.md](../SECURITY.md).

## Finding each other without a server

Every EchoFog install has an identity: a key pair created on your computer and backed up by your
recovery phrase. Your **peer ID** (the Relay card in **Proxy** shows it as *Your peer ID*) is a fingerprint of your
public key (a SHA-256 hash, shown as 64
hex characters). Nobody hands IDs out, so nobody can take yours away, and anyone who talks to you
can check that the other end really holds the key behind that ID.

To find a peer by its ID, nodes use a **DHT** (distributed hash table, here Kademlia): a shared
address book split across all the nodes. Each node keeps a small part of it and knows a few
neighbours "closer" to any given ID, so a lookup hops from node to node until it reaches someone
who knows the answer. Public exits also publish their offers (price, speed) in the DHT, which is how the
VPN market works without a website.

A fresh install has to know at least one node to join. Those are **bootstrap nodes** (also called
seed nodes). A release can carry a built-in list, but **the list may be empty in a given build**,
and built-in nodes can be blocked. So you can add your own: in the app, **Settings → Bootstrap
nodes**, paste a node link (`echofog://relay/…`) or `host:port/<peer id>` from a friend or from the
project's channel <!-- TODO(owner): link -->. The peer ID is required, because each node's traffic is
disguised with a key tied to its ID (see the next section). Once you are connected to anyone, the
DHT gives you the rest of the network.

## Getting through the censor

Censors use DPI (deep packet inspection: equipment that looks inside traffic to recognise and block
protocols). EchoFog tries three ways to get through, in this order.

1. **QUIC over UDP, obfuscated.** QUIC is the fast, modern transport also used by HTTP/3. On top of
   it, every UDP packet is scrambled under a key derived from the receiving node's ID, so packets
   look like random bytes. There is no key shared by the whole network and none is baked into the
   program.
2. **TCP that looks like a browser visiting a real HTTPS site.** When UDP is blocked, the app falls
   back to TCP. It opens the connection with a genuine Chrome TLS greeting (made with the uTLS
   library, so its fingerprint matches real Chrome) and hides a small secret token inside it, in
   the style of REALITY. A server node checks the token. Anyone who connects **without** the token,
   such as a censor's probe, is quietly passed through to the real website the node imitates and
   sees that site's real certificate and pages. Such visitors are never counted, slowed down or
   banned, so probing does not single the node out.
3. **WebSocket behind a CDN.** A node can also sit behind a CDN such as Cloudflare, so the censor
   sees a connection to the CDN and to the operator's own domain.

A desktop app is a pure client: it opens no inbound TCP listener at all, so there is nothing on
your computer for a prober to find. Public server nodes must imitate a plausible site in their own
datacenter; they can find one automatically. The details, and how to run a node so the disguise
holds, are in [anti-dpi.md](anti-dpi.md).

**The limits, honestly:**

- The per-node key only stops **blind** scanning, where the censor sees an IP address but does not
  know the node's ID. Public nodes are listed in the DHT with their address and ID together. A
  censor who crawls the DHT can learn both, compute the token, and confirm that an address is an
  EchoFog node.
- The answer is **hidden and isolated nodes**: they are not in the DHT, never tell other nodes
  their address, and answer nobody who cannot prove the access secret from their invitation link
  (`echofog://join?…`). People hand the links out themselves. See [hidden-nodes.md](hidden-nodes.md).
  For a whole private network there are also the `anti_dpi.obfs_password` / `stealth_password`
  settings.
- Some planned hardening is not in this build either (for example, a client presenting each target
  node's own imitated site name). [anti-dpi.md](anti-dpi.md) lists it.

## Connecting through home routers

Most computers sit behind **NAT**: your router sharing one public address among your devices. Two
people behind NAT cannot simply dial each other. EchoFog tries, in order:

- **Direct connection**, when one side has a public address.
- **Port mapping** with UPnP or NAT-PMP: the app politely asks your router to open a port for it.
- **Hole punching**: both sides send packets to each other at the same moment, coordinated through
  a node both can reach, so each router thinks the other side's packets are replies. **Signaling
  nodes** (`echofog-node run --signaling`) exist for this: they keep the DHT and coordinate hole
  punching, but carry no traffic and keep no messages. Hole punching works through most home
  routers ("cone" NAT) but not through a "symmetric" NAT, which picks a new public port for every
  destination.
- **A relay**: a public node that carries the (still encrypted) traffic between the two of you when
  nothing else works.

To find out what kind of NAT you are behind, the app asks several public STUN servers what address
they see. Chatting, calls and using the VPN all work even if your computer cannot be reached from
the Internet; only serving others as a relay or public exit needs it. A **hidden** exit serves the
people it invited even from behind a router, through the relays named in its link. If you want to be
reachable, see [reachability.md](reachability.md).

## Messages

Messages are **end-to-end encrypted**: only you and the person you write to can read them. Nodes
and relays in between see only scrambled bytes.

In plain words, the scheme (the same family as Signal's) is:

- **X3DH** — the first time you write to someone, your app and theirs agree on a shared secret
  using each other's public keys, even if the other person is offline at that moment.
- **Double Ratchet** — after that, every message is locked with a fresh key, and old keys are thrown
  away. If one key ever leaks, it does not unlock past messages, and the conversation heals itself
  for future ones.

**Offline delivery.** Your app keeps each message until the recipient's app sends back an encrypted
receipt, and resends it with growing pauses (for up to 7 days). If the recipient is offline, a relay
they recently used can hold the encrypted message in a **mailbox** and hand it over when they come
back (also up to 7 days). A relay only keeps mail for people who have used it recently, and has size
limits per person and in total, so strangers cannot fill its disk.

**Groups** have no server either. Every group message and every change (new member, role, ban) is
sent to each member over that member's own encrypted session, with the same receipts and offline
queue as a normal chat. The person who created the group is its owner; members can be admins,
moderators, ordinary members or restricted. Changes are signed, and a member cannot post in another
member's name.

## Calls and screen sharing

Calls are free and end-to-end encrypted: audio and video are encrypted with a key for that call
(AES-256-GCM), and media is never sent or accepted without it.

There is no central media server. Instead, the people in the call carry each other's streams in a
**relay tree**. The person sharing their screen sends it to a few participants; each of them
forwards it to a few more, and so on. How many others a person feeds depends on **their own measured
upload speed** — the app measures real throughput — so a fast connection does more work and a slow
one does less. Only people who actually watch a stream receive it. Voice uses a separate, shallow
tree, so speech stays fast.

That is why **4K at 60 fps** is possible without a company's servers: the 4K60 preset is about
25 Mbit/s, and the sender only has to push it to the first few people in the tree, not to everyone.
The available presets go from 360p30 up to 4K60; the app checks that your upload can feed at least
one person at the quality you pick.

## The VPN

You go online through another person's computer: an **exit**. There are two ways to use it, on the
**Proxy** screen:

- **SOCKS5 for apps** — a local proxy that you point a browser or another app at. Only those apps go
  through the exit.
- **Route the whole computer through the exit** — a TUN device (a virtual network card) takes the
  traffic of every app. It asks for administrator rights. On Windows the driver it needs (Wintun) is
  built into the app and written out when needed, so there is nothing extra to install. **There is no
  kill switch yet:** if EchoFog or its helper stops unexpectedly, your apps fall back to your normal
  connection and your real IP can be exposed for a moment. DNS is kept inside the tunnel on a
  best-effort basis.

**Exits are on an open market.** Anyone can share their connection, set a price per GB (or 0 for
free) and some free test traffic, and publish the offer in the DHT. Running an exit means other
people's traffic leaves from your IP address. You can also connect to one specific node, for example
a friend who shares their connection with you for free (**Specific node**).

**Hidden exits are not on the market.** An exit can also be hidden: it publishes nothing and serves
only the people it gave an invitation link to (`echofog://join?…`), at its price. You use one by
pasting the link into **Specific node**. See [hidden-nodes.md](hidden-nodes.md).

**How an exit is chosen automatically.** In **Automatic** mode the app checks exits in the
background. For each one it opens your sites for real through that exit (by default Cloudflare,
Wikipedia and Google; you choose your own list, up to 16 sites, under **Check sites**) and measures
the round trip and the speed. It then ranks exits by:

- how often the exit answered,
- ping,
- how many of your sites opened,
- whether the measured speed matches what the exit claims,
- price.

An exit that failed several checks in a row drops far down. The ratings stay on your computer and
are never published. In **Choose myself** mode you can sort the list by ping, price or speed and pick
one.

**What an exit refuses to reach.** Every exit, public or hidden, only lets traffic out to the Internet. It refuses:

- its own machine and network: loopback, private and link-local ranges, carrier-grade NAT space, the
  subnets of its own network interfaces (on IPv6 that covers the whole home network), and its own
  public IP addresses (which would loop back into the operator's router);
- reserved and special-purpose ranges (documentation, benchmarking, NAT64, 6to4 and similar);
- ports used for spam and worms: 25, 465 and 587 (mail sending), 135, 137–139 and 445 (Windows file
  sharing), and 1900 (SSDP).

## Paying

There is no account and no commission. You keep TON in your own small vault contract, and while you
use a paid exit your app signs small vouchers for what you use; the exit collects them every few
hours. Paying by direct TON or USDT transfer also works. Calls, chat and free exits need none of
this. The full explanation is in [vault-payments.md](vault-payments.md) (Russian:
[vault-payments.ru.md](vault-payments.ru.md)). The vault is on testnet and has not been audited yet.
