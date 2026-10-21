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
# signaling only (DHT + help with hole punching, no traffic) — cheap; or false for a full relay
ECHOFOG_SIGNALING=false
ECHOFOG_RELAY=true
# share this server's connection as a paid exit (other people's traffic leaves from its IP)
ECHOFOG_EXIT=false
ECHOFOG_ADVERTISE_PORT=443
# empty for the very first node; otherwise the join address of a node already running
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
docker compose -f deploy/docker-compose.yml up -d --build
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

The node accepts TCP only while it imitates a real HTTPS site plausible for its IP. It looks for
one in its own subnet by itself; inside Docker it often cannot, so it runs on UDP only and logs an
`ANTI-DPI` warning. To turn TCP on, set a real TLS 1.3 site hosted in the same datacenter and its
IP in `deploy/.env`, then run `up -d` again:

```bash
ECHOFOG_TLS_SERVER_NAME=<site domain>
ECHOFOG_CAMOUFLAGE_ADDR=<site IP>:443
```

Details: [Anti-DPI](anti-dpi.md).

## Update

```bash
cd ~/EchoFog && git pull          # the folder you cloned in step 3
cd echofog-core-oss && docker compose -f deploy/docker-compose.yml up -d --build
```

The identity, database and wallet live in the `echofog-data` volume and survive updates.
