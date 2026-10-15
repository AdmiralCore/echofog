# Anti-DPI: camouflage, roles, and what each defence does and does not do

EchoFog's TCP/WebSocket fallback disguises itself as a browser visiting a large HTTPS site: a
genuine Chrome TLS 1.3 ClientHello (uTLS), a REALITY-style stealth token hidden in the
`legacy_session_id`, and transparent forwarding of anyone without a valid token to the real site, so
a prober sees that site's certificate and content, never ours. UDP is QUIC with per-packet
obfuscation. This document explains how to run a node so that camouflage actually holds against real
DPI (TSPU, GFW), and is honest about the limits of each defence.

## Two roles: client and public node

A node is one of two things for anti-DPI purposes, decided entirely by whether it exposes a public
inbound listener.

### Client (the common case)

A desktop user behind NAT/CGNAT. It sets **no** `network.tcp_listen_addr` and **no**
`network.ws_listen_addr`. It raises no inbound TCP/WS listener at all, so it never presents a server
certificate or a mimicked SNI to a prober. When UDP is blocked it dials **out** to a public node
over TCP/WS. A client needs no domain, no certificate and no `anti_dpi` camouflage settings — it
works out of the box.

### Public node (relay / exit / bridge)

A node that accepts inbound TCP or WS connections. `echofog-node` does so by default: its TCP
listener sits on the QUIC port (`--listen-addr`) unless `--tcp-listen` moves it (e.g. `:443`) or turns
it `off`. A listener bound to loopback (`127.0.0.1`, `::1`) does not count — that is the CDN /
reverse-proxy setup, where the world sees the CDN's IP and your own domain. A public listener **is**
visible to active probes, so it must mimic a real site that is plausible for its own IP:

- `anti_dpi.tls_server_name` — the SNI it presents and mimics.
- `anti_dpi.camouflage_addr` — the real `host:port` a prober is forwarded to.

**A public node opens its TCP/WS listener only once both are set.** Until then it runs on QUIC/UDP
only (which needs no camouflage), logs a loud `ANTI-DPI` warning, and keeps looking for a neighbour
every 30 minutes; the TCP listener comes on by itself, without a restart, when one is found. There is
no baked-in default
SNI: a shared default such as `www.microsoft.com` served from a residential or VPS IP is itself an
SNI/IP-mismatch that DPI flags (`www.microsoft.com` does not live on a Hetzner or DigitalOcean IP).

#### Automatic neighbour discovery (default)

If a public node starts with `anti_dpi.tls_server_name` / `camouflage_addr` **unset**, it discovers
a neighbour to mimic automatically, REALITY-style:

1. It finds its own public IPv4 address (the kernel's egress address; on a VPS this is the public
   IP). If that address is private/CGNAT, discovery is skipped — its neighbours are not real sites.
2. It gently scans nearby addresses in its own `/24` (±20 around its own host octet, in random
   order, at most 4 connections in flight, a 100–200 ms pause between launches) and **stops at the
   first** site that negotiates TLS 1.3 **and** whose ServerHello (version, cipher, key-share group)
   matches ours. This gentleness is deliberate: hosters such as Hetzner, OVH and DigitalOcean strike
   a VPS for aggressive outbound port scanning.
3. It adopts that site's certificate domain as the SNI and its `ip:443` as the forward target, and
   **caches the result to disk** (`anti_dpi_neighbour.json` in the profile directory). Later restarts
   re-check the cached neighbour and skip scanning entirely while it still holds.

If no nearby site answers like us (for example none offers the same post-quantum key-share group),
nothing is adopted and the node runs on UDP only (see above), retrying later. To get TCP at once, set
a neighbour by hand.

#### Choosing the mimicked site manually

Pick a real site **hosted in your node's own datacenter / ASN**, ideally in the same `/24`, and set
both fields (this also skips the scan):

```json
"anti_dpi": {
  "tls_server_name": "a-real-neighbour.example",
  "camouflage_addr": "a-real-neighbour.example:443"
}
```

Then the SNI you present matches a real site whose IP is in the same network as yours, and a prober
you forward reaches that real site and sees its genuine certificate.

Optionally serve a **real** certificate for a domain you control with `anti_dpi.tls_cert_file` /
`anti_dpi.tls_key_file`; without one the node uses a self-signed certificate. In TLS 1.3 the
certificate is encrypted, so a *passive* observer never sees it — it only matters to an active prober
that both forges a valid token *and* completes the handshake, which the stealth secret (below) is
what actually prevents.

## The stealth/obfs key, and what the per-node key protects

Each node derives its stealth-token key and its UDP-obfuscation key from its **peer ID**
(`NodeStealthKey`, `NodeKey`). This means a client that knows a node's ID (because it was given the
node's address *and* ID) can compute the token; a scanner that only sees the IP cannot.

**Limitation — read this if your node serves censored users.** A node's peer ID is public whenever
its address and ID are published together (e.g. in the open DHT, or a bootstrap list). A censor that
crawls that mapping can forge a valid token for the node and reach our own TLS instead of being
forwarded to the real site — confirming the node — or de-obfuscate its QUIC. **The per-node key only
stops *blind* scanning of an IP whose ID the scanner does not know.** It is not sufficient on its own
for a node whose job is to carry traffic for people behind the GFW/TSPU.

For such nodes:

- **Do not publish address + ID together in the public DHT.** Distribute the node's address and ID
  **out of band** (bridge-style: handed to specific users through a side channel), so a censor
  crawling public data cannot enumerate `(address, ID)` pairs.
- **Set a shared secret** (`anti_dpi.obfs_password`, or `anti_dpi.stealth_password` for the stealth
  token specifically). This mixes a secret only your users know into both keys, so even someone who
  learns the ID cannot forge a token or de-obfuscate without the secret. Treat it as mandatory for
  bridge-style operation. Do not bake a network-wide secret into a release — each private
  deployment chooses its own.

(The bridge-mode out-of-band distribution and the matching config surface are being built in a
follow-up; this document states the operational rule so nodes run safely in the meantime.)

## First-flight length padding

The self-signed camouflage certificate is padded to a realistic, per-node-randomised encoded size.
In TLS 1.3 the Certificate message is encrypted, but a passive observer can still measure the
record's length; a tiny fixed-size certificate would be a first-flight length fingerprint that the
mimicked site does not have. Padding is per node, so there is no single network-wide "EchoFog
certificate size" either.

## What the node checks for you

A public node probes the mimicked site once at start-up and logs whether its own ServerHello
(TLS version, cipher, chosen key-share group) matches the real site's. If the log warns that they
differ, choose a different SNI whose server answers like ours, or the passive fingerprint will not
match.

## Forwarded visitors are never singled out

A connection that does not present a valid stealth token is forwarded to the real site and is
**never** counted, rate-limited or banned, and the forwarded path uses an idle (not absolute)
timeout. A real site does not reset ordinary HTTPS clients or hang up after a fixed time; a prober
opening a handful of connections, or a NAT/CGNAT behind which many clients share one address, must
look exactly like traffic to that site. The per-address guard (rate limit and ban) applies only
after a valid token, and a ban is earned only by a failed *authenticated* handshake.

## Planned follow-up work (not yet in this build)

- **Per-peer SNI distribution**: a dialing client should present the *target node's* SNI, carried in
  the node's card alongside its address and ID, instead of its own. Until this lands, a client's
  outbound fallback uses its own configured SNI.
- **UDP first-packet length padding** toward the mimicked site's sizes. QUIC Initials are already
  padded to ~1200 bytes by the QUIC stack; safe additional padding at the obfuscation layer needs a
  QUIC-aware change and is deferred rather than shipped fragile.
