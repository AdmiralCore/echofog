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

A lookup asks up to 16 nodes at once, the ones it is already connected to first, and stops as
soon as somebody hands the record over or once the closest nodes have all answered without it.
A node that does not answer at its address is dropped from the table and left alone for ten
minutes, however often other nodes still name it: the network is small and clients come and go,
so without that memory every lookup would wait on the same departed peers. What a lookup came to
is kept for a short while (half a minute for a record found, a quarter for a key nobody held);
**Refresh** on the exit list walks the network again regardless.

A fresh install has to know at least one node to join. Those are **bootstrap nodes** (also called
seed nodes). A release can carry a built-in list, but **the list may be empty in a given build**,
and built-in nodes can be blocked. So you can add your own: in the app, **Settings → Bootstrap
nodes**, paste a node link (`echofog://relay/…`) or `host:port/<peer id>` from a friend or from the
project's [channel](https://t.me/EchoFogDev). The peer ID is required, because each node's traffic is
disguised with a key tied to its ID (see the next section). Once you are connected to anyone, the
DHT gives you the rest of the network.

## Getting through the censor

Censors use DPI (deep packet inspection: equipment that looks inside traffic to recognise and block
protocols). It checks which protocol a connection speaks, which site it names and whether that site
lives at that address, what the server does when the censor connects to it himself, and how the
connection behaves over time.

- **QUIC that looks like HTTP/3.** A public node can imitate a real site in its own datacenter.
  A client connecting to it sends the same QUIC greeting Chrome sends to that site, with that
  site's name in it. A small secret token, tied to that exact greeting, tells the node who is a
  client. Anyone without it, such as a censor's probe, is passed on to the real site and gets the
  real site's answer.
- **TCP that looks like a browser visiting the same site**, for networks that block UDP: a genuine
  Chrome TLS greeting (made with the uTLS library), the token hidden inside it, and the same pass-on
  for anyone without the token. Such visitors are never counted, slowed down or banned, so probing
  does not single the node out.
- **Both at once.** The app dials QUIC and TCP together and keeps the first that connects. If QUIC
  connects and then goes quiet, it moves to TCP. It remembers what worked on each network, so the
  next connection there goes straight to it.
- **Inside the tunnel**, the first few kilobytes are padded, so the handshake of the HTTPS site you
  open through the VPN does not show through by its sizes. Connections to an exit are replaced every
  few minutes or tens of megabytes, so no single one runs for an hour carrying gigabytes, which
  browsers do not do.
- **WebSocket behind a CDN.** A node can also sit behind a CDN such as Cloudflare, so the censor
  sees a connection to the CDN and to the operator's own domain.

A desktop app is a pure client: it opens no inbound port at all, so there is nothing on your
computer for a prober to find. The details, the lab measurements, and how to run a node so the
disguise holds are in [anti-dpi.md](anti-dpi.md).

**The limits, honestly:**

- Only the path from you to the exit is disguised. Traffic between nodes (the DHT, relays) is
  scrambled so that it looks like random bytes, and GFW has a rule for exactly that kind of traffic.
- The token key is derived from the node's ID, and public nodes are listed in the DHT with their
  address and ID together. A censor who crawls the DHT can compute the token and confirm that an
  address is an EchoFog node.
- The answer is **hidden and isolated nodes**: they are not in the DHT, never tell other nodes
  their address, and answer nobody who cannot prove the access secret from their invitation link
  (`echofog://join?…`). People hand the links out themselves. See [hidden-nodes.md](hidden-nodes.md).
  For a whole private network there are also the `anti_dpi.obfs_password` / `stealth_password`
  settings.
- Everything above was tested in a lab, not yet against a real national DPI.

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
back (also up to 7 days). Each app picks up to three relays it is connected to as its mailboxes and
names them in its signed key record, so a sender that cannot reach you takes the mail straight to
one of those. A relay only keeps mail for people who have used it recently (it remembers them across
a restart), and has size limits per person and in total, so strangers cannot fill its disk. If the
recipient vanished without closing the connection, the relay notices the silence within seconds and
keeps the message instead of pushing it into a dead link.

**The chat history** is a log of events inside the encrypted session: messages, replies, threads,
edits, deletions, reactions and pins. When two apps connect they exchange what the other missed,
so a chat looks the same on both sides after a break. The contact list shows who is online right
now, and a call invitation that reaches you more than two minutes late shows as a missed call
instead of ringing.

**The first message to somebody you never met** needs their public keys. The app looks them up in
the DHT; when the DHT does not answer (a small network, a table full of nodes that are gone), it
sends its own signed keys to the recipient the same way as any message, through a relay and its
mailbox, and the recipient answers with theirs. So a contact added by key gets your message even
when the two of you have never been online at the same time and the lookup fails.

**What the marks in a chat mean.** Under a message of yours: a clock means the app is waiting for
the recipient's keys or for a way to reach them (they are offline and no relay has taken the message
yet), one check means the recipient or a relay took it (a pale check: it is waiting at a relay),
two checks mean the recipient's app confirmed
it. The same states tell you what a call you placed is waiting for, until it connects.

**Spaces.** A group can have text channels and voice rooms next to its main chat, like a
community server, but there is no server. Channels are parts of the group's one encrypted log, so
they share its members, roles and history. Admins add, rename, reorder and remove them, give a
channel a topic, or make it an announcements channel where only admins post and the others comment.
A voice room is a call that stays open: you see who is in it and join when you like, and nobody's
phone rings. In a space with voice rooms, calls happen in the rooms, so its header has no call
buttons.

**Threads.** Any message can get comments. In a space each channel lists its threads, and the
author of a message (or a moderator) can give its thread a name.

**Who may do what in a group** is part of the group's log too: the creator writes down who is in
the group, and every join, role change, removal and ban after that is an entry in the log. Each
device checks a message against the members and roles at the point where it sits in the log, so
all devices agree even if entries arrive in a different order. A member who leaves keeps what they
wrote while they were in, and someone who joins later sees it.

**Contacts.** You can rename a contact and give them a picture. Both stay on your device; the
contact is not told. Their profile also lists the photos, files and links from your chat.

**Pictures.** When you send a photo (JPEG, PNG or GIF), your app makes a small preview, about 6 KB,
and sends it with the file offer. The other side sees the picture right away, before downloading
the file or without downloading it at all.

**Groups** have no server either. Every group message and every change (new member, role, ban) is
sent to each member over that member's own encrypted session, with the same receipts and offline
queue as a normal chat. On top of that, a message is signed by its author and sealed once for the
whole group with a key that admins replace whenever somebody leaves or is removed; so any member
can hand a message on to one the author could not reach, and nobody can change who wrote it,
history included. The person who created the group is its owner; members can be admins,
moderators, ordinary members or restricted, or hold roles the group defines itself. The details are
in [Group encryption](group-encryption.md); channels, roles, invitations and moderation are in
[Groups and spaces](groups.md).

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
35 Mbit/s, and the sender only has to push it to the first few people in the tree, not to everyone.
The available presets go from 360p30 up to 4K60, with 1080p and 1440p also at 120 fps; their
bitrates are what H.264 needs for a moving picture, and a still screen takes much less. The app
checks that your upload can feed at least one person at the quality you pick, and watches the real
path: when it backs up, the encoder is asked for a little less, and gets back up only once the path
has stayed clear for a couple of seconds. Viewers hold frames for a tenth of a second before showing
them, so frames that cross relays unevenly still play evenly.

A participant nobody can reach directly (both sides behind strict NATs, say) is reached through a
relay circuit, and the call stays one call. Its sound and video cross the relay as datagrams, as
they do on a direct link, so one lost packet does not hold up the ones behind it. A public relay
gives each peer up to 50 Mbit/s by default: enough for one 4K60 stream, tight for several. Calls do not go through the VPN exit, so
the exit's speed limit does not touch them.

## The VPN

You go online through another person's computer: an **exit**. There are two ways to use it, on the
**Proxy** screen:

- **SOCKS5 for apps** — a local proxy that you point a browser or another app at. Only those apps go
  through the exit.
- **Route the whole computer through the exit** — a TUN device (a virtual network card) takes the
  traffic of every app. It asks for administrator rights. On Windows the driver it needs (Wintun) is
  built into the app and written out when needed, so there is nothing extra to install. It goes to
  `C:\Program Files\EchoFog`, where only administrators can change it, and nowhere next to the exe:
  the app can be run from Downloads or the desktop. **There is no
  kill switch yet:** if EchoFog or its helper stops unexpectedly, your apps fall back to your normal
  connection and your real IP can be exposed for a moment. DNS is kept inside the tunnel on a
  best-effort basis.

**Exits are on an open market.** Anyone can share their connection, set a price per GB (or 0 for
free) and some free test traffic, and publish the offer in the DHT. Running an exit means other
people's traffic leaves from your IP address. You can also connect to one specific node, for example
a friend who shares their connection with you for free (**Specific node**).

A free exit gives each client up to 50 Mbit/s by default (100 Mbit/s for all clients behind one IP
address), so one person cannot take the whole connection; paying clients and the operator's own
devices are not limited. The operator can change this.

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

## Protecting the nodes

Anyone can make as many identities as they like, so a node never trusts that one identity is one
person: it limits peers and identities per address and per subnet, bans peers that misbehave, asks
strangers for a proof of work and answers a forged address only after a QUIC Retry, and gives every
peer only a share of its queue, its relay and its exit. Requests that could make it send much more
than it received (speed probes, peer lists) have budgets for the whole node, and everything it
stores for others (offline mail, DHT records, keys handed out) expires and is bounded. A node on a
VPS runs in the server role: no chat, calls or file transfer, so strangers cannot make it decrypt or
store anything for them. More in [Protecting a node](node-security.md).

## Paying

There is no account and no commission. You keep TON in your own small vault contract, and while you
use a paid exit your app signs small vouchers for what you use; the exit collects them every few
hours. Paying by direct TON or USDT transfer also works. Calls, chat and free exits need none of
this. The full explanation is in [vault-payments.md](vault-payments.md) (Russian:
[vault-payments.ru.md](vault-payments.ru.md)). The vault is on testnet and has not been audited yet.
