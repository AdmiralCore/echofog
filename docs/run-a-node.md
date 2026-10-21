Русская версия: [run-a-node.ru.md](run-a-node.ru.md)

# Run a node

EchoFog has no company servers. The network is the people who run it. If you have a VPS (a rented
server with a public IP) or a home computer that stays on, you can help by running `echofog-node`,
the headless node.

> **Status: experimental.** EchoFog has not yet had an independent security audit by a specialist
> organization, and it has not been tested against a real national DPI. Do not put more TON into a
> node's wallet than you are ready to lose.

## Pick a role

| Role | How to turn it on | What it does | Bandwidth |
|---|---|---|---|
| **Signaling node** | `--signaling` | Keeps the DHT (the shared address book of the network) and helps two peers behind routers connect directly to each other (hole punching). Carries no calls, files or messages and keeps no messages. | Almost none |
| **Public relay** | default (`--relay` is on) | Carries calls and messages for peers that cannot connect directly, and keeps messages for people who are offline. This is what makes a call work between two people who are both on mobile internet or behind CGNAT. | Real bandwidth: every relayed call goes in and out through your server |
| **Private relay** | `--relay-private --relay-allow <peer id>,<peer id>` | A relay only for the peer IDs you list. It is not advertised in the network. Good for yourself and a few friends. | As much as your friends use |
| **Exit** | `--exit` (add `--exit-price`, `--exit-free-kb`) | Shares your Internet connection as a VPN exit. Other people's traffic leaves the Internet **from your IP**. | As much as your customers use |
| **Hidden or isolated exit** | `--exit --access hidden` or `--exit --access isolated` | An exit that is not listed anywhere and serves only the people you give its invitation link to. Isolated also joins no network at all. See [Hidden and isolated nodes](hidden-nodes.md). | As much as your people use |

You can combine a relay with an exit. `--relay=false` turns relaying off. A hidden or isolated node
is never a relay: `--access hidden|isolated` turns relaying off whatever `--relay` says.

### About running an exit

Be clear about what this means: websites, your provider and the police see the traffic of your
exit's users as coming from your IP address. **The legal responsibility is yours.** Only run an exit
on a server and in a country where you may do this, and read your hoster's terms first.

- `--exit-price 0.05` sets the price in dollars per GB, paid in TON or USDT into the node's own
  wallet. `0` (the default) means free. How the payments work: [vault-payments.md](vault-payments.md).
- `--exit-free-kb 512` is the free test traffic per client per day, in KB (0–1024, default 512). It
  applies when the exit is priced.
- `--exit-upload-mbps` sets the upload speed to advertise. Without it the node measures it against
  connected peers first, which fails on a fresh server with no peers yet: set it there (e.g. 100).
- `--exit-free-peers <id>,<id>` (`ECHOFOG_EXIT_FREE_PEERS`) lists peers served for free, with no
  daily quota and no payment, such as your own devices. It replaces the free list saved in the data
  directory (the desktop app edits the same list under **Proxy → Free for these people**).
- `--access hidden` or `--access isolated` keeps the node and its exit to the people you give an
  invitation link to: nothing is advertised. See [Hidden and isolated nodes](hidden-nodes.md).
- An exit never connects to loopback, private/LAN or CGNAT addresses, or to this machine's own
  networks. It also refuses these destination ports: **25, 465, 587** (mail: they would get your IP
  onto spam blocklists) and **135, 137, 138, 139, 445, 1900** (Windows networking, SMB and SSDP:
  worm and scanning targets).

## Get the binary

- **Prebuilt:** CI builds `echofog-node` for Linux (amd64) and Windows (amd64) on every push to
  `main`; a version tag also makes a release with a `SHA256SUMS` file. Download it from the Releases
  page. <!-- TODO(owner): releases link -->
- **From source** (Go 1.27+):

  ```bash
  cd echofog-core-oss
  make build            # -> bin/echofog-node
  ```

## First run

```bash
echofog-node run --data-dir ./echofog-data --bootstrap <host:port/peer id>
```

On the first start the node creates a new identity and prints a **recovery phrase** once, in a box.
Write it down and keep it offline. It restores the node's identity **and its TON wallet**. If you
prefer, create the identity first with `echofog-node init`, then start with `run`.

To move a node to another server, or after losing the data directory:

```bash
echofog-node restore --data-dir ./echofog-data      # asks for the phrase, the words are hidden
```

`--mnemonic-file` reads the phrase from a file instead. `restore --force` replaces an existing
identity (its messages and contacts are deleted).

Where things live:

- `--data-dir` (default `/data`, env `ECHOFOG_DATA_DIR`): the database, the identity and the wallet.
  Back it up.
- The identity is encrypted with a password. If you give none (`--password` or, better,
  `--password-file`), the node generates one and saves it in `<data-dir>/.node_password`, so it can
  restart unattended. This means anyone who can read the data directory has the node's identity and
  wallet: protect it like a key.

Every flag is also an environment variable: `--exit-price` is `ECHOFOG_EXIT_PRICE`, `--data-dir` is
`ECHOFOG_DATA_DIR`, and so on. `echofog-node run -h` lists all flags.

## Open the ports

The node listens on `--listen-addr` (default `:4242`):

- **UDP 4242** — QUIC, the main path: messages, calls, relayed and VPN traffic.
- **TCP 4242** — the TCP fallback, for people whose network blocks UDP. It listens on the same port
  as QUIC by default. `--tcp-listen :443` moves it to another port; `--tcp-listen off` turns it off.

Open both in the server's firewall and in your hoster's firewall panel. For example with `ufw`:

```bash
sudo ufw allow 4242/udp
sudo ufw allow 4242/tcp
```

If the public port differs from the one the node listens on (for example a Docker port map
publishes 443 to the node's 4242), tell the node with `--advertise-port 443` (and
`--tcp-remote-port 443` for the TCP fallback).

## Anti-DPI camouflage, in plain words

The TCP fallback pretends to be a browser visiting a real HTTPS site. To be believable, the site it
pretends to be should live right next to your server, in the same network. Details:
[anti-dpi.md](anti-dpi.md).

- **Automatic (default).** A public node looks for a real HTTPS site among its neighbours in its own
  `/24` (the 256 addresses around its IP). It scans gently, so the hoster does not take it for a port
  scan, and remembers the site it found.
- **If no suitable neighbour is found,** the node still runs, but **on UDP only**: people whose
  network blocks UDP cannot reach it. It logs a loud `ANTI-DPI` warning and tries again every 30
  minutes. When it finds a site, TCP comes on by itself, without a restart.
- **By hand.** Pick a real site hosted in the same datacenter, ideally in the same `/24`, and set:

  ```bash
  echofog-node run --tls-server-name a-real-neighbour.example \
                   --camouflage-addr a-real-neighbour.example:443
  ```

  This also skips the scan. `--tls-cert` / `--tls-key` let you serve a real certificate for a domain
  you own; without them a self-signed one is used.

**Cloud VMs with a private interface address.** On some providers the VM's own network interface
has a private address and the public IP is NATed in front of it (Google Cloud works this way).
There the node cannot see its public neighbours, so automatic discovery is skipped and the node
stays on UDP only. Set `--tls-server-name` and `--camouflage-addr` by hand to get TCP.

## Join the network

- `--bootstrap host:port/<peer id>` (comma-separated for several) names the nodes to join through.
  The host may be a domain. The peer ID is required: each node's traffic is disguised with a key tied
  to its ID. This open-source build has no built-in seed nodes unless they were added at build time,
  so pass at least one. <!-- TODO(owner): list of public seed nodes / where to find them -->
  The very first node of a new network starts without `--bootstrap`.
- At startup the node prints its **Join address**: `host:port/<peer id>`. Until the node has learned
  its public IP, the host shows as a placeholder; `echofog-node status` shows it later.
- Share it with others. Another `echofog-node` uses it in `--bootstrap`. Desktop users add it in
  **Settings → Bootstrap nodes** (“Add and connect”), either as is or as a link:

  ```
  echofog://relay/<host:port>/<peer id>
  ```

- A **hidden or isolated** node turns away whoever does not have its link, so give out its
  **invitation link** instead (`echofog-node invite`). An isolated node ignores `--bootstrap`: it
  joins no network. See [Hidden and isolated nodes](hidden-nodes.md).
- For a **private relay**, give your friends this address too. In the desktop app they add it under
  **Proxy → Relay → Relays you use**; another node uses `--relays host:port/<peer id>`. You add their
  peer IDs to `--relay-allow`.

## Bandwidth and cost

A signaling node costs almost nothing. A relay is different: it forwards calls, and video uses a
lot of outbound traffic. A 4K60 screen share is tens of Mbit/s **per viewer** it is relayed to. Many
cloud providers charge per GB of outbound traffic, and that adds up fast.

- For a **relay** (or an exit), prefer a VPS plan with generous or unmetered traffic.
- Keep **metered cloud VMs as signaling nodes** (`--signaling`).
- `--relay-max-circuits` (default 200) limits how many relayed connections run at once.

## Check on it

```bash
echofog-node status          # peers, NAT, relay traffic, exit sessions and earnings, wallet
echofog-node wallet          # the TON wallet address and balance
echofog-node status --json   # raw JSON
echofog-node invite          # the invitation link of a hidden or isolated node
echofog-node invite --list   # who joined with it (--rotate: a new link, --remove <peer id>: cut off)
```

These ask the node's status endpoint, which listens on `127.0.0.1:7070` (`--status-addr`;
`''` disables it). Keep it on loopback: it shows the wallet address and earnings, and it only
answers requests addressed to localhost. `status --quiet` prints nothing and returns an exit code,
for health checks. The node also logs a stats line every minute (`--stats-every`).

## Docker

The repository has a `Dockerfile` and a compose file in `echofog-core-oss/deploy/`:

```bash
cd echofog-core-oss
cp deploy/.env.example deploy/.env       # then edit it
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs -f          # the recovery phrase is printed here on first start
docker compose -f deploy/docker-compose.yml exec node echofog-node status
```

The compose file publishes host port **443** (UDP and TCP) to the container's 4242, so that the node
blends in with ordinary HTTPS and HTTP/3; `.env.example` sets `ECHOFOG_ADVERTISE_PORT=443` to match.
Open 443/udp and 443/tcp in the firewall. The identity, database and wallet live in the
`echofog-data` volume: back it up.

For an exit only for the people you invite, set `ECHOFOG_ACCESS=isolated` (or `hidden`) in
`deploy/.env` and take the link with
`docker compose -f deploy/docker-compose.yml exec node echofog-node invite`. Step by step:
[VPS with Docker](deploy-vps.md#an-exit-only-for-the-people-you-invite).

## Behind a home router?

A node at home works for itself, but to serve others as a relay or public exit it must be reachable
from the Internet. See [reachability.md](reachability.md). A **hidden** exit is the exception: it
serves the people it invited from behind a router, through the relays named in its link (see
[Hidden and isolated nodes](hidden-nodes.md)).

## Built-in limits against abuse

A peer ID costs nothing to make, so every limit below holds for all peers together, not only per
peer. The defaults suit a VPS; none needs setting.

| What | Limit |
| --- | --- |
| Speed probes other peers ask for (12 MB each) | one per peer every 5 s, two at once, 48 MB a minute in all. `--bwprobe-mb-per-min` changes the budget (below 12 turns answering off); `--no-bwprobe-serve` turns it off. Measuring exits yourself is not affected. |
| Peer lists | answered only to the connected peer asking, at most 16 peers, once per peer every 10 s, 120 answers a minute; lists are taken in only when this node asked, and only public addresses |
| Known-peer store | peers only heard of are no longer saved past 2000 entries |
| Offline messages | expired ones are purged every 10 minutes and stop counting against quotas; at most 100 000 recipients remembered |
| DHT records | deleted when their own TTL ends; at most 10 000 held and 5000 new ones an hour |
| Prekeys handed out on request | unused ones go after 72 hours; at most 2048 kept, oldest first out |
