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
`network.ws_listen_addr`, opens no inbound port, and so never shows a certificate or a site name to
a prober. It only dials out. A client needs no domain, no certificate and no `anti_dpi` settings.

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
itself, without a restart, once one is found. There is no built-in default site: a shared name like
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

Nodes without a site to imitate, and the links between nodes (DHT, peer exchange, relays), use the
older per-node obfuscation: every packet is scrambled under a key derived from the receiving node's
ID, so it looks like random bytes, and a stranger's handshake gets silence. There is no key shared
by the network and none in the program.

Random-looking UDP is exactly what GFW's "fully encrypted traffic" rule blocks. So today the client
to exit path is disguised, while the mesh between nodes is not. In the lab that mesh traffic is
small (0.1 MB out of 2.2 GB in the last run), but a node inside a censoring country can still be
picked out by it. Moving the mesh onto the disguised transport is the next piece of work.

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
With no camouflage exit left, the VPN reports the failure instead.

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

## Not working yet: Russia's 16 KB freeze

TSPU in Russia has been seen to freeze a TCP connection to a foreign datacenter after roughly
16 KB. The config has a switch for a defence, `anti_dpi.micro_chunk_tcp`: carry the tunnel in
chunks under that size over several connections and put them back together at the exit. **In this
build the switch does nothing:** the chunk format is written and tested, but neither the client nor
the exit uses it yet. A new connection every 15 KB would also cost too much speed for video, so the
plan is to use it only as a slow last resort, after QUIC and an entry node inside the country.

## Setting up a public node

### Automatic neighbour discovery (default)

If `tls_server_name` and `camouflage_addr` are not set, a public node finds a site to imitate by
itself:

1. It finds its own public IPv4 address. If that is private or CGNAT, it stops: its neighbours are
   not real sites.
2. It looks at nearby addresses in its own `/24` (20 on each side of its own, in random order, at
   most 4 at a time, 100-200 ms apart) and stops at the first site that speaks TLS 1.3 and whose
   ServerHello (version, cipher, key-share group) matches its own. It goes slowly on purpose:
   Hetzner, OVH and DigitalOcean act against servers that scan ports.
3. It takes that site's certificate name as its SNI and its `ip:443` as the forward address, and
   saves both to `anti_dpi_neighbour.json`. After a restart it checks the saved neighbour again
   and skips the scan while it still answers.

If no neighbour fits, the node stays on UDP only and keeps trying.

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
`flow_shaping` (the defaults above), `region_aware_obfs` (off), `home_country`,
`allow_noise_to_servers` (off) and `micro_chunk_tcp` (off) stay at their defaults on a server. The desktop app reads them from its
config.

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
