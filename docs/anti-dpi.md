Русская версия: [anti-dpi.ru.md](anti-dpi.ru.md)

# Anti-DPI: how the disguise works and where it stops

A censor's DPI box looks at a few things: which protocol a flow speaks, which site name (SNI) it
asks for and whether that site really lives at that address, how the TLS greeting is built, what
the server does when the censor connects to it himself, and the shape of the flow over time (how
long, how heavy, what sizes at the start). This page goes through what EchoFog does about each, how
to set up a public node so the disguise holds, and what is still missing.

Everything measured here was measured in the lab (a Docker network with a censor that reads the
captures the way GFW and TSPU are known to). EchoFog has not yet been tested against a real
national DPI. The numbers are in [Measurements](#measurements) below.

## Two roles: client and public node

### Client (the common case)

A desktop user behind NAT or CGNAT. It sets no `network.tcp_listen_addr` and no
`network.ws_listen_addr`, so it runs no TCP or WebSocket listener and never shows a certificate or a
site name to a prober. It does have a UDP socket for QUIC (`network.listen_addr`, a random port by
default), and with `anti_dpi.obfs=node` that socket answers nobody who does not know its peer ID: a
stranger's handshake gets silence. By default the client asks the router to forward that port
(`network.port_mapping`, UPnP or NAT-PMP) and takes part in hole punching (`network.hole_punch`),
so peers can reach it. `network.anonymous`, and `network.proxy` unless calls are set to direct,
turn both off, and STUN with them. A client needs no domain, no certificate and no `anti_dpi`
settings.

### Public node (relay, exit, bridge)

A node that accepts inbound connections. `echofog-node` does by default: its TCP listener sits on
the QUIC port (`--listen-addr`) unless `--tcp-listen` moves it (e.g. `:443`) or turns it `off`. A
listener on loopback (`127.0.0.1`, `::1`) does not count: that is the CDN or reverse-proxy setup,
where the world sees the CDN's address and your domain.

A public port can be probed, so the node imitates a real site that is plausible for its own
address:

- `anti_dpi.tls_server_name` (`--tls-server-name`): the site name it presents.
- `anti_dpi.camouflage_addr` (`--camouflage-addr`): the real `host:port` of that site, where
  visitors without the secret are sent.

The same pair is used on TCP and on UDP. A node with both set is a **camouflage node**. It puts the
site name in its identity card, and other nodes pass it on with its address
(`host:port/<peer id>?sni=<site>`), so a client knows which site to pretend to visit before it
dials.

A public node opens its TCP listener only once it has a site to imitate. Until then it runs on UDP
only, logs an `ANTI-DPI` warning, and looks for a neighbour every 30 minutes; TCP comes on by
itself, without a restart, once one is found. A public WebSocket listener (`network.ws_listen_addr`
not on loopback) also stays off while there is no site, and comes on only at the next start. There is no built-in default site: a shared name like
`www.microsoft.com` coming from a Hetzner or home address is itself a mismatch a censor can flag.

## What a censor sees on the wire

### UDP to a camouflage node: HTTP/3 to the imitated site

A client that dials a camouflage node sends a real QUIC v1 handshake shaped like Chrome's: the same
extension order, GREASE values, the post-quantum key share Chrome sends, and a first packet of 1250
bytes. The SNI is the node's site. A censor that decrypts the first packet (QUIC hides it only
under keys anyone can compute, and GFW and TSPU do compute them) reads an ordinary HTTP/3 visit.

The client proves it knows the node's ID with a token hidden in the ClientHello's random field.
The token is tied to the key share of the same ClientHello, so it cannot be lifted into another
handshake; a replayed or altered handshake is treated like any stranger.

A stranger (a probe, a scanner, a real browser) is forwarded packet for packet to the site's own
UDP port and gets the site's real HTTP/3 answer and certificate. If the site does not speak HTTP/3,
the stranger gets no answer, which is also what that site does on its own. At most 48 strangers are
forwarded at once; past that, the one that has been quiet longest is dropped.

### UDP to an ordinary node: noise

A node dials another node over the disguised QUIC above whenever it knows that node's site name,
whatever the link carries (DHT, peer exchange, relays). A link to a node whose site name it does
not know uses the older per-node obfuscation: every packet is scrambled under a key derived from
the receiving node's ID, so it looks like random bytes, and a stranger's handshake gets silence.
There is no key shared by the network and none in the program.

Random-looking UDP is exactly what GFW's "fully encrypted traffic" rule blocks. Guard mode (below)
keeps a user's node from crossing a censoring border with it: there the node dials only
camouflaged servers. Noise is still left on links to nodes with no site name (users' own computers
have none), and on a user's links when guard mode is off or has stood down. In the lab that traffic
is small (0.1 MB out of 2.2 GB in the last run), but a node inside a censoring country can still
be picked out by it when it runs without guards.

### TCP: a browser visiting the site

When UDP does not get through, the client uses TCP that starts like Chrome visiting the site: a
genuine Chrome TLS 1.3 ClientHello (uTLS, with GREASE and the post-quantum key share), a token in
the session ID field tied to that ClientHello, and the site's name. The node checks the token; a
connection without it is passed to the real site, which answers with its own certificate and pages.

### WebSocket behind a CDN

A node can also sit behind a CDN such as Cloudflare. The censor then sees a connection to the CDN
with the operator's own domain.

## When UDP is blocked or throttled

The client dials QUIC and TCP together and takes whichever connects first. QUIC gets a 300 ms head
start; on a network where QUIC failed before, TCP goes first. If QUIC connects and then goes
silent (some censors let the handshake through and drop what follows), the client moves to TCP and
tries TCP first at that address for the next 10 minutes.

The client remembers which transport won on which network for 30 minutes. A network is told apart
by the set of the computer's interfaces and addresses, so moving from home Wi-Fi to a phone's
hotspot starts fresh. The memory is kept in `transport-memory.json` in the node's database folder
and survives a restart.

In the lab, with UDP dropped at the border, the first page through the VPN came after 0.8 s. Before
the race was added it took 37 s.

## Inside the tunnel: hiding the inner handshake

When you open an HTTPS site through the VPN, your browser's own TLS handshake travels inside
EchoFog's encrypted connection. Its sizes and timing still show through: a ClientHello of about
517 bytes going up, a few kilobytes of certificate coming down. Censors look for that pattern
("TLS in TLS") to spot proxies.

EchoFog carries the first ~4 KB in each direction of every tunnel in padded frames: small records
are blown up to a random 900-1100 bytes, larger ones get a little random extra. After that the data
goes through as it is, so a long download pays nothing. Both ends must support it, and a node
announces that it does; with an old node the tunnel stays unpadded. Setting: `anti_dpi.tunnel_padding`
(on by default).

## The shape of the flows

One QUIC connection that runs for an hour and carries gigabytes does not look like a browser.
EchoFog keeps a small pool of connections to an exit (up to three) and keeps renewing it:

- a new connection is dialed after a random 15-45 MB or 2-5 minutes on the current one, from a new
  source port, and no new stream starts on a connection older than a minute: a fresh one is dialed
  first;
- new streams always go to the newest connection;
- an older connection is closed within a second of carrying nothing, the first one too;
- a connection that is carrying a download is never closed under it, so a big file is never broken
  by the renewal; it is closed once the download ends.

So between downloads the client keeps a single young connection, and the extra ones exist only
while something is still running on them. Both ends must support the pool, and a node announces
that it does; with an old exit the client stays on one connection.

If the exit stops answering, the VPN moves to another exit, but a client that was on a camouflage
exit only ever moves to another camouflage exit, never to an ordinary node whose traffic is noise.
With no other camouflage exit, the VPN keeps its proxy up and redials the exit in use every 15
seconds until it, or another camouflage exit, answers; connections through it fail meanwhile.

In the lab, over three minutes of heavy downloading, no EchoFog connection both lived past 2
minutes and carried 20 MB. The settings are under `anti_dpi.flow_shaping` (`max_connections`,
`max_bytes_min`/`max_bytes_max`, `max_conn_lifetime_min`/`max_conn_lifetime_max`,
`min_dial_interval`).

## Optional: cheaper links inside one country

With `anti_dpi.region_aware_obfs` on, links that stay inside the node's own country (or on the
local network) use the noise obfuscation, which costs less, and links that cross a border keep the
disguise. A peer whose country cannot be placed keeps the disguise. It is **off by default**,
because inside a country that also censors domestic traffic, noise is exactly what gets flagged.
`anti_dpi.home_country` (e.g. `"DE"`) fixes the node's own country when the GeoIP lookup is blocked.

## Russia's 16 KB freeze

TSPU in Russia has been seen to freeze a TCP connection to a foreign datacenter once the server has
sent about 16 KB on it: the connection stays open and goes silent. QUIC (UDP) is not hit by it. The
test bed reproduces it (`testbed/censor/freeze.go`, a router rule), and without a defence a client
whose network lets only TCP through gets nothing: no page, no download.

Three things answer it, in this order:

1. **QUIC first.** The node dials QUIC and falls back to TCP only when QUIC does not get through.
   Where a TCP link has to rotate (below), the preference for TCP lasts two minutes, not ten, so
   QUIC is tried again soon.
2. **A guard inside the country.** Behind the freeze (the node's own country is Russia), guard mode
   picks guards in the node's own country first. That link never crosses the border; the guard
   relays what has to. It needs the GeoIP of the guard's address (or `anti_dpi.home_country` for
   the node itself).
3. **Link rotation, the last resort** (`anti_dpi.tcp_rotate`, flag `--tcp-rotate`). One TCP link is
   carried by many short TCP connections. Each side counts what it put on a connection, the TLS
   handshake included, and moves to a spare connection before 12 KB; the streams above do not
   notice. A server takes rotating links from any client. `on` rotates every TCP link the node
   dials, `off` never, `auto` (the default) rotates links that cross the border from Russia, and the
   links to an address where a plain link went silent after a few KB (learned for six hours, which
   costs the first link 30 to 75 seconds in the test bed). `micro_chunk_tcp` true now means `on`.

Rotation costs a connection every 8-10 KB, so it is slow. The number of spare connections follows
the speed (two while the link is quiet, up to twelve while data waits for one). In the test bed it
gives 5-12 MB/s at a 1 ms round trip and about 1 MB/s at 50 ms, against 80 MB/s for a plain TCP
connection with no freeze. On a real network the round trip is what counts. It is for messages,
pages and slow video, not for 4K.

## Setting up a public node

### Automatic neighbour discovery (default)

If `tls_server_name` and `camouflage_addr` are not set, a public node finds a site to imitate by
itself:

1. It finds its own public IPv4 address. On a cloud machine or in Docker the interface has a private
   address, so it takes the one from `public_addrs` (`ECHOFOG_PUBLIC_ADDR`) or the one STUN observed
   instead. With neither, or behind CGNAT, it stops: its neighbours are not real sites. STUN answers
   a little after start-up, so on such a machine the first scan may come with the retry 30 minutes
   later; `ECHOFOG_PUBLIC_ADDR` makes it start at once.
2. It looks at nearby addresses in its own `/24` (20 on each side of its own, in random order, at
   most 4 at a time, 100-200 ms apart) and stops at the first site that speaks TLS 1.3 and whose
   ServerHello (version, cipher, key-share group) matches its own. It goes slowly on purpose:
   Hetzner, OVH and DigitalOcean act against servers that scan ports.
3. It takes that site's certificate name as its SNI and its `ip:443` as the forward address, and
   saves both to `anti_dpi_neighbour.json`. After a restart it checks the saved neighbour again
   and skips the scan while it still answers.

Only a site with a certificate from a publicly trusted CA for a real domain name counts. A
self-signed certificate, an internal name (a Kubernetes API server answers as
`kubernetes.default.svc`) or an IP address in place of a name is not a site browsers visit.

If no neighbour fits, the node stays on UDP only and keeps trying every 30 minutes. From the first
retry on it also searches wider, at most every 12 hours: every address of its `/24`, then of the two
`/24`s on each side (about 1 300 addresses, at the same slow pace, a few minutes in all).
`echofog-node camouflage` runs the same search at once and prints what it finds, with the lines for
`deploy/.env`.

### Choosing the site by hand

Pick a real site hosted in your node's own datacenter, ideally in the same `/24`, and set both
fields (this also skips the scan):

```json
"anti_dpi": {
  "tls_server_name": "a-real-neighbour.example",
  "camouflage_addr": "a-real-neighbour.example:443"
}
```

On `echofog-node` these are `--tls-server-name` and `--camouflage-addr`. A site that also speaks
HTTP/3 is better: then a probe on UDP gets an answer too.

Optionally serve a real certificate for a domain you own with `anti_dpi.tls_cert_file` and
`anti_dpi.tls_key_file`; without one the node uses a self-signed certificate. In TLS 1.3 the
certificate is encrypted, so only a prober that already has a valid token ever sees it.

### Hand out the address with the site name

When you give someone the address of a camouflage node, add the site name:
`host:port/<peer id>?sni=a-real-neighbour.example`. Then their very first connection is already
disguised. Without it the first connection goes out as noise, and the client learns the site name
only after it has joined. The `Join address` that `echofog-node` prints already has `?sni=` when
the node has a camouflage site, and invitation links of hidden nodes carry it too.

A server's site name, and the fact that it is a server (`&srv=1` in the address), travel with its
address everywhere: the join address, invitation links (also the relays inside them), bootstrap and
relay lists, peer exchange, the DHT and the exit market. A node also tells every peer it exchanges
peers with how it is to be dialed, and each node keeps what it learned across restarts. Before this
an address learned from a third party could arrive without the name, or with `?sni=...` stuck to
the `host:port` so it could not be dialed at all.

### A client does not dial a server across a border over noise

A server announces the server role in its handshake (`CapServer`) and in its address. A client that
knows a server's site name dials it disguised. If it knows the node is a server but not its site
name (an old address, a list that lost it), it does **not** dial it with noise across a border: it
waits for the name, which arrives with the next peer exchange, or does not dial. A country that
cannot be placed counts as across the border; addresses on the local network, and (with
`region_aware_obfs`) in the node's own country, are exempt. A server, in turn, does not dial a
user's node over noise: users dial servers. `anti_dpi.allow_noise_to_servers` switches the rule off
for people who run their own servers without a cover site, from a network that does not censor.
Addresses typed in by hand (`ConnectToPeer`) are the user's own choice and are dialed as given.

### Guards: a user's node keeps few links across the border

A user's node does not build a mesh of its own across a censoring border: every link it dials there
is a flow a censor can look at. It holds two or three **guards** instead, camouflaged servers (a
site name known, reached over REALITY on QUIC or browser TLS on TCP), and takes the rest of the
network from them. Peer exchange runs over the guards. A DHT lookup asks the guards, which walk the
DHT themselves and answer with the record or the closest peers they reached (the protocol is
unchanged; an older server answers from its own table, as before). Peers the node dials itself —
peer exchange, the maintainer, a lookup's referrals — are dialed across the border only if they are
guards. What the user asked for (a contact, a call, a relay, an exit) is dialed as before.

Guards are picked from the bootstrap nodes and the known servers with a site name, spread over
networks, and kept for 30 to 60 days. A guard that is gone for 90 seconds gets a stand-in beside it;
one that stays gone for 20 minutes while the node still has other links is dropped. With no guard up
the node may dial any camouflaged server (never over noise) until it has guards again, so a failing
guard never leaves it without a network. The choice survives restarts.

`anti_dpi.guard_mode` (`--guard-mode`, `ECHOFOG_GUARD_MODE`): `on` engages it on every node that is
not in the server role; `off` leaves the mesh dialing as it was. `auto` (the default) goes by the
country of the node's own public address (or `anti_dpi.home_country`): it engages in a country on
the built-in list of censoring ones (Russia, Belarus, Iran, China and others, in
`pkg/api/node/guard_auto.go`) and while the country is not known yet, and stands down elsewhere.
Once a censoring country has been seen, it stays on until the node restarts. `auto` needs
`anti_dpi.obfs=node`; with obfuscation off it never engages. The country check runs in the desktop
app only; on `echofog-node`, `auto` on a node that is not a server acts like `on`. Pinned guards
(`anti_dpi.pinned_guards`: relays the user chose, held as guards for as long as they are pinned)
turn it on under `auto` even in a country that does not censor, and `network.anonymous` set to
`relay` makes the node connect to nothing but them. Local-network addresses, and with
`region_aware_obfs` the node's own country, are dialed directly either way; a country that cannot
be placed counts as across the border. A guard adds one hop to a lookup and does not sit in the
data path of a VPN tunnel, which runs straight to the exit.

**What a guard sees.** A guard is a server someone else runs, and it is placed to watch you. It
sees your IP address (unless you come through Tor), the keys you look up in the DHT through it, and
so which contacts and exits you resolve, and the peer IDs, timing and volume of what it carries for
you. It cannot read messages, which are end-to-end encrypted, but it can hold back answers or hand
you only the peers it chooses. Spreading guards over networks and keeping them for weeks limits
how many servers get this view; pinning a relay you run yourself keeps the view with you.

### Users across the border meet at a relay

In guard mode two users' nodes do not link directly across a border: a direct link is noise or a
hole punch, a flow of its own between two strangers. A call, a stream, a file or a message to a
user whose address is across the border goes through a **circuit on a camouflaged relay** instead:
one of the node's guards if the peer is connected to it too, else one of the peer's own relays (the
mailbox relays named in its record, with their site names), which the node then connects to as a
server. The node does not answer a hole-punch request from such a peer either. A node the user
listed (an invitation, a bootstrap entry) is linked to directly, and so are peers on the local
network and, with `region_aware_obfs`, in the node's own country. Relaying is free within the
relay's limits; a relay may carry a price in its advert (`relay_price_per_mb`, 0 = free), which
clients note and nothing bills yet.

### What the node checks for you

At start-up a public node connects to the imitated site once and logs whether its own ServerHello
(TLS version, cipher, key-share group) matches the site's. If the log says they differ, choose
another site, or a passive observer can tell the two apart.

The self-signed certificate is padded to a realistic size that differs from node to node, so there
is no "EchoFog certificate length" to look for either.

### Visitors without the secret are never singled out

A connection without a valid token is forwarded and is never counted, slowed down or banned, and
the forward uses an idle timeout, not a fixed one. A real site does not reset ordinary visitors or
hang up after a set time, and a probe, or a CGNAT with many people behind one address, must see
exactly what that site shows. Limits and bans apply only after a valid token, and only a failed
*authenticated* handshake earns a ban.

## The per-node key and its limit

Each node derives its token key and its obfuscation key from its **peer ID**. A client that knows
the ID (because it was given address and ID together) can compute the token; a scanner that sees
only the address cannot.

**Read this if your node serves people in a censored country.** Public nodes are listed in the DHT
with address and ID together. A censor that crawls the DHT can compute the token for any of them,
reach the node's own TLS instead of the imitated site, and so confirm the node. The per-node key
only stops *blind* scanning.

For such nodes:

- **Keep the node out of the DHT.** Run it with `--access hidden` or `--access isolated`
  (`ECHOFOG_ACCESS` in Docker). Its address and ID then reach people only in its invitation link,
  which also carries an access secret the peer must prove in the handshake; without it the node
  sends nothing back, not even its public key. Replace a leaked link with `invite --rotate`. See
  [Hidden and isolated nodes](hidden-nodes.md).
- **For a whole closed network, also set a shared secret** (`anti_dpi.obfs_password`, or
  `anti_dpi.stealth_password` for the token only). It is mixed into both keys, so knowing the ID is
  not enough. It is not in the invitation link: every node and app of that network must be set up
  with it. Do not build a network-wide secret into a release.
- **Keep obfuscation on** (`anti_dpi.obfs=node`, the default). With it off, a scanner that
  completes the QUIC handshake can read the node's public key from its certificate.

## Settings that `echofog-node` cannot change yet

`echofog-node` builds its settings from flags and has no config file. So `tunnel_padding` (on),
`flow_shaping` (the defaults above), `region_aware_obfs` (off), `home_country` and
`allow_noise_to_servers` (off) stay at their defaults on a server. `micro_chunk_tcp` cannot be set
either, but `--tcp-rotate on` does the same. The desktop app reads them from its config.

## Measurements

Three lab runs in a row on 27 October 2026 (commit `285aea1`, `go run ./testbed/launchpad run
-censor-for 3m`): three clients behind three different routers used the VPN through a camouflage
exit for three minutes, about 2 GB in all, while a browser visited the imitated site as a control.
A censor program read the captures at every border. The table shows the last run; the first
line shows all three.

| What the censor tried | Result |
|---|---|
| EchoFog bytes in flows a GFW-like rule set would flag | 0.0 / 0.0 / 0.1% (25 Oct, before this work: 100%) |
| QUIC flows to the exit read as ordinary HTTP/3 | 39 of 44; the other 5 are mesh links under 150 KB, not tunnels |
| TCP flows to the exit read as ordinary TLS | 36 of 36 |
| Mesh links from the clients' networks to other nodes | 15, 1.3 MB, all noise |
| VPN moved to another exit during the run | never |
| Proxy spotted by the inner TLS handshake (24 HTTPS visits) | 0 (QUIC before padding: 10 of 12) |
| A probe's HTTP/3 request to the exit | same certificate and page as the real site |
| A probe's QUIC handshake to an ordinary node | no answer |
| Connections longer than 2 min **and** heavier than 20 MB | 0 |
| First page through the VPN with UDP dropped | 0.7 s (before the transport race: 37 s) |
| First page with 70% of UDP lost | 0.4 s |
| A 300 MB download in one stream while connections rotate | whole, 54 s (5.6 MB/s) |
| 72 downloads at once | 72 of 72 |

What is left is the mesh noise described above. It is a small share of the bytes, but a censor
that blocks flows one by one counts flows, not bytes. In the lab only the exit has a camouflage
site; real public nodes find one on their own, so on a real network fewer of these links are
noise. The censor program reads what is
publicly known about GFW and TSPU; a real DPI may look at things it does not.
