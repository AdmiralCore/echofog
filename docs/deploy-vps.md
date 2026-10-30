Русская версия: [deploy-vps.ru.md](deploy-vps.ru.md)

# Running a node on a VPS with Docker

Any VPS with a public IP and Ubuntu or Debian will do. This page is the short recipe; what the
roles and settings mean is in [Run a node](run-a-node.md).

## 1. Connect and install Docker

```bash
ssh root@<server IP>
```

```bash
curl -fsSL https://get.docker.com | sh
```

If you log in as a regular user instead of root, add yourself to the `docker` group and log in
again: `sudo usermod -aG docker "$USER"`.

## 2. Open the port

The node uses one port for UDP (QUIC) and TCP (the fallback for networks that block UDP). The
compose file publishes it as **443**. If the server runs `ufw`:

```bash
ufw allow 443/udp && ufw allow 443/tcp
```

If your provider has its own firewall in the control panel, open UDP and TCP 443 there too. **UDP
443 is the one that matters**: QUIC, which carries almost all traffic, runs over it, and cloud
firewalls usually only have TCP open. On Google Cloud:

```bash
gcloud compute firewall-rules create echofog --allow udp:443,tcp:443
```

## 3. Get the code and configure

```bash
git clone https://github.com/AdmiralCore/EchoFog.git
cd EchoFog/echofog-core-oss
cp deploy/.env.example deploy/.env
nano deploy/.env
```

The settings that matter most:

```bash
# server: relay, mailboxes for others, DHT, exit; no chat, calls or files of its own
ECHOFOG_ROLE=server
# signaling only (DHT + help with hole punching, no traffic). Leave it false: without a relay
# people behind NATs cannot write to or call each other. Cap the relay with ECHOFOG_RELAY_MBPS instead.
ECHOFOG_SIGNALING=false
ECHOFOG_RELAY=true
# share this server's connection as a paid exit (other people's traffic leaves from its IP)
ECHOFOG_EXIT=false
ECHOFOG_ADVERTISE_PORT=443
# empty: join through the built-in node; or join addresses of nodes you know (they replace it)
ECHOFOG_BOOTSTRAP=
```

`ECHOFOG_ROLE=server` is what a VPS needs: the node runs the network services and none of the
messenger ones (chat, contacts, calls, file transfer), and drops frames of those kinds on arrival
without decrypting or storing them. Nobody can chat with or call the node itself, but messages
between other people still pass through it and wait in its mailbox while the recipient is
offline. The compose file uses `server` even when `.env` has no `ECHOFOG_ROLE` (an older copy);
set `ECHOFOG_ROLE=client` only if you want to use the node as your own messenger too. Details:
[Server or client](run-a-node.md#server-or-client).

## 4. Start

```bash
docker compose -f deploy/docker-compose.yml pull
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml logs -f
```

On the first start the node prints a **recovery phrase** — write it down and keep it offline; it
restores the node's identity and wallet. It also prints a **Join address**:
`<this server's public IP or domain>:443/<peer id>`. Put the server's IP in front: that is what
other nodes use in `ECHOFOG_BOOTSTRAP` and what app users paste into **Settings → Bootstrap
nodes**.

The startup summary in the log and
`docker compose -f deploy/docker-compose.yml exec node echofog-node status` show `Role: server`.

Check that it is healthy:

```bash
docker compose -f deploy/docker-compose.yml ps
```

## Sharing the connection as an exit

To let people (or just your own devices) browse through this server, set in `deploy/.env`:

```bash
ECHOFOG_EXIT=true
ECHOFOG_EXIT_PRICE=0.05          # dollars per GB; 0 = free for everyone
ECHOFOG_EXIT_UPLOAD_MBPS=100     # roughly what the server has; never 0 on a fresh server
# your own devices ride free, without the daily quota (peer ID: app, Proxy → Relay → Your peer ID)
ECHOFOG_EXIT_FREE_PEERS=<peer id>,<peer id>
```

With `ECHOFOG_EXIT_UPLOAD_MBPS=0` the node measures its speed against other peers first; on a new
server there are often none yet and the log says `Exit sharing not started: no peers to measure
upload speed against`. On a priced exit everybody not on the free list gets 512 KB of free test
traffic a day, then has to pay.

The stats line in the log counts refused streams by reason, e.g. `exit: 12 sessions, 30 refused
(payment required 28, dial failed 2)`, and each refusal is logged as `refused stream` (at most once
per reason every 30 seconds). The app shows on its Proxy page why an exit does not serve it: the
exit is off, the free traffic is used up, or the exit does not answer.

**A priced exit needs a little TON on the node's own wallet.** Clients usually pay from their
deposit, and collecting that money is a transaction whose fee the node's wallet pays (about
0.015 TON each). With an empty wallet the payments wait and the stats line says `vouchers: … owed,
… cashed (the node wallet has no TON …)`. Put 0.1–0.2 TON on the address from `echofog-node status`
(on testnet: Telegram bot @testgiver_ton_bot). Details: [Getting paid](vault-payments.md#put-a-little-ton-on-the-nodes-wallet-first).

## An exit only for the people you invite

To keep the exit out of the public list and serve only the people you give a link to, add to
`deploy/.env` (next to `ECHOFOG_EXIT=true`):

```bash
ECHOFOG_ACCESS=isolated          # or hidden: see below
```

then `docker compose -f deploy/docker-compose.yml up -d` and take the link:

```bash
docker compose -f deploy/docker-compose.yml exec node echofog-node invite
```

- **isolated**: the node joins no network and dials nobody; people with the link connect straight
  to the published port. The usual choice for a VPS.
- **hidden**: the node still uses the network as a client (useful when the port cannot be reached,
  as the link then also names a relay), but is not listed and relays for nobody.

The link uses the server's public IP (from STUN) with `ECHOFOG_ADVERTISE_PORT` (443 in this
compose). For a domain, or a server with several IPs, set `ECHOFOG_PUBLIC_ADDR=vpn.example.org:443`.
The secret is generated on the first start and kept in the `echofog-data` volume, so the link
survives restarts and updates. `invite --rotate` makes a new link, `invite --list` shows who
joined, `invite --remove <peer id>` cuts somebody off. To run several servers behind one link, give
them the same secret: make one with
`docker compose -f deploy/docker-compose.yml run --rm node invite --new-secret` and put it in
`ECHOFOG_ACCESS_SECRET` (or a Docker secret with `ECHOFOG_ACCESS_SECRET_FILE`, see the compose
file). The price stays `ECHOFOG_EXIT_PRICE`; `ECHOFOG_EXIT_UPLOAD_MBPS` is not needed. Details:
[Hidden and isolated nodes](hidden-nodes.md).

## TCP fallback

The node imitates a real website from its own datacenter: clients reach it the way Chrome would
open that site, and a censor who connects without the secret lands on the real site. You do not
need a domain for this. The node finds such a site by itself: first among the 20 addresses on each
side of its own, at start-up; if none fits, it searches the whole `/24` and the `/24`s next to it in
the background (at most every 12 hours, slowly, since hosters dislike port scans). It only takes a
site with a publicly trusted certificate for a real domain that answers TLS 1.3 exactly like the
node does. Until it has one it runs on UDP only and logs an `ANTI-DPI` warning; it turns TCP on by
itself once it finds one.

To see the sites at once, or to choose one yourself, run in `deploy/`:

```bash
docker compose run --rm node camouflage
```

It probes the same addresses (a few minutes) and prints the two lines for `deploy/.env`:

```bash
ECHOFOG_TLS_SERVER_NAME=<site domain>
ECHOFOG_CAMOUFLAGE_ADDR=<site IP>:443
```

Then run `docker compose up -d` and send people the new link from `invite`. If it finds nothing,
there is no suitable site near your server. A site of your own then works too: point a domain at
another small server in the same datacenter and serve it over HTTPS with a real certificate (Caddy
does this by itself).

The same two lines turn on the UDP disguise too: clients will reach the node the way Chrome would
open that site over HTTP/3. If the site itself speaks HTTP/3, a probe on UDP gets its real answer.
Invitation links of a hidden node carry the site name by themselves; give out a public address as
`<IP>:443/<peer id>?sni=<site domain>&srv=1` so that the first connection is disguised as well.

Details: [Anti-DPI](anti-dpi.md).

## Hardening

The setup above already runs the node in the server role (`ECHOFOG_ROLE=server`): no chat, calls or
file transfer, only what a relay and an exit need. Limits against floods, free identities and abuse
are on by default. Two things are worth setting yourself in `deploy/.env`:

- `ECHOFOG_RELAY_MBPS` below your plan's uplink, so relayed traffic leaves room for the exit and
  the node itself;
- `ECHOFOG_WS_CLIENT_IP_HEADER=CF-Connecting-IP` if WebSocket goes through Cloudflare and its port
  is reachable only through the CDN.

Keep only the node's port open in the provider's firewall. What each protection does, how to see it
in `status` and the log, and what it does not cover (a flood that fills the uplink needs the
provider's DDoS protection): [Protecting a node](node-security.md).

Every limit of the relay and the exit (circuits, speeds, daily and monthly caps of a free exit) is a
line in `deploy/.env.example`; what each one does is in
[Run a node → Relay and exit limits](run-a-node.md#relay-and-exit-limits). A few other settings:

| Variable | What it does |
| --- | --- |
| `ECHOFOG_PASSWORD_FILE` | the node's password from a file (a Docker secret); without it one is generated and kept in `/data/.node_password` |
| `ECHOFOG_MNEMONIC_FILE` | restore an existing identity from its 12 or 24 words on the first start |
| `ECHOFOG_TON_NETWORK` | `testnet` (the default while the vault contract is unaudited) or `mainnet` |
| `ECHOFOG_STATS_EVERY` | how often the stats line goes to the log (`1m`): peers, relayed traffic, messages kept for offline peers, earnings |
| `ECHOFOG_LOG_LEVEL` | `debug`, `info`, `warn` or `error` |

## Update

```bash
cd ~/EchoFog && git pull          # the folder you cloned in step 3; brings a changed compose file
cd echofog-core-oss
docker compose -f deploy/docker-compose.yml pull
docker compose -f deploy/docker-compose.yml up -d
```

The identity, database and wallet live in the `echofog-data` volume and survive updates.

### The ready-made image

The compose file runs `ghcr.io/admiralcore/echofog-node:latest`, which GitHub builds from every
push to `main` (for `amd64` and `arm64`), so the server downloads about 30 MB and compiles nothing.
A build is ready about ten minutes after the push; until then `pull` keeps the previous one. Every
build is also tagged with its commit, e.g. `sha-4d6bfaa`: put
`ECHOFOG_IMAGE=ghcr.io/admiralcore/echofog-node:sha-4d6bfaa` in `deploy/.env` to stay on one build or
to go back to it.

`main` is the stable branch: what goes there was tested first. New work lands in `dev` and is built
as `ghcr.io/admiralcore/echofog-node:dev`. Use it on a server you test on
(`ECHOFOG_IMAGE=ghcr.io/admiralcore/echofog-node:dev`), not on one people depend on.

To build from your checkout instead (needs about 4 GB of free disk), add the build file:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.build.yml up -d --build
```

If a build stops with `no space left on device`, old build layers filled the disk:
`docker builder prune -af` and `docker image prune -af` free them without touching the node's data.

## Clearing stale data

The node cleans up after itself: DHT records expire, peers that stay unreachable are forgotten,
mail older than a week is dropped. When it holds something stale anyway (a node that moved or got
a new key, a lot of departed peers after a big test), drop what it learned from the network; it
learns it again within minutes:

```bash
cd ~/EchoFog/echofog-core-oss
docker compose -f deploy/docker-compose.yml stop node
docker compose -f deploy/docker-compose.yml run --rm node clean            # --dry-run to count first
docker compose -f deploy/docker-compose.yml up -d
```

This drops DHT records, known peers and their addresses, exit ratings and cached key bundles. It
never touches the identity, the wallet, the exit's ledger, the access secret or who joined a
hidden node. `--mail` also drops the mail kept for offline people (they lose it), `--bans` the bans
of misbehaving peers. Do not remove the `echofog-data` volume to "clean": that throws away the
node's key, and every link and client that knows it stops working.
