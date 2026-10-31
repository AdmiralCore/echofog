Русская версия: [node-cli.ru.md](node-cli.ru.md)

# The node command line: every command and flag

`echofog-node` is what runs on a server (in Docker or directly). With Docker Compose the container
runs `echofog-node run`; every other command is run next to it. This page lists them all. The
same list is always available from the binary: `echofog-node help`, `echofog-node <command> -h`.

Every flag can also be set with an environment variable: the flag name in capitals, dashes turned
into underscores, prefixed with `ECHOFOG_` (`--exit-price` is `ECHOFOG_EXIT_PRICE`). In Docker
Compose put them under `environment:` of the `node` service in `deploy/docker-compose.yml`.

## Docker cheat sheet

All commands are run on the server in the folder that holds `deploy/`. `exec` talks to the running
node; `run --rm` starts a one-off container on the same data, so use it for commands that need the
node stopped.

| I want to | Command |
|---|---|
| start / stop / restart | `docker compose -f deploy/docker-compose.yml up -d` / `stop node` / `restart node` |
| see the log (the recovery phrase is printed on the first start) | `docker compose -f deploy/docker-compose.yml logs -f node` |
| see what the node does: peers, relayed traffic, earnings | `docker compose -f deploy/docker-compose.yml exec node echofog-node status` |
| the same as JSON | `... exec node echofog-node status --json` |
| health check (exit code only) | `... exec node echofog-node status --quiet` |
| the TON wallet address and balance | `... exec node echofog-node wallet` |
| get the invitation link of a hidden or isolated node | `... exec node echofog-node invite` |
| list who joined with the link | `... exec node echofog-node invite --list` |
| make a new link (the old one lets nobody new in) | `... exec node echofog-node invite --rotate` |
| remove a person from the list | `... exec node echofog-node invite --remove <peer id>` |
| make a fresh access secret (needs no running node) | `... run --rm node invite --new-secret` |
| clear what the node learned from the network (stale peers, DHT records, ratings) | `docker compose -f deploy/docker-compose.yml stop node`, then `... run --rm node clean`, then `... up -d` |
| the same, also dropping mail kept for offline people and the bans | `... run --rm node clean --mail --bans` (add `--dry-run` to only count) |
| version | `... exec node echofog-node version` |
| update | `docker compose -f deploy/docker-compose.yml pull` then `up -d` |
| start from nothing: delete the node's data (identity, wallet, everything) | `docker compose -f deploy/docker-compose.yml down` and then remove the data volume (`docker volume ls` shows it, named like `deploy_echofog-data`; then `docker volume rm <name>`). **This destroys the identity and the wallet: write down the recovery phrase first.** |
| restore an identity from its recovery phrase | stop the node, put the phrase in a file and run `... run --rm -v $PWD/phrase.txt:/phrase.txt node restore --mnemonic-file /phrase.txt` (add `--force` to replace an existing one) |
| create the identity without starting | `... run --rm node init` |

`...` stands for `docker compose -f deploy/docker-compose.yml`. Without Docker, run the same
commands directly: `echofog-node status`, `echofog-node clean`, and so on.

## Commands

| Command | What it does |
|---|---|
| `run` | start the node (the default when no command is given) |
| `init` | create a new identity and print its recovery phrase |
| `restore` | restore an identity from its recovery phrase |
| `status` | show what the running node does |
| `wallet` | show the TON wallet address and balance of the running node |
| `invite` | the invitation link of the running node (hidden and isolated nodes) |
| `pass` | personal links of a hidden node with a term, devices and traffic (list, new, show, extend, revoke, delete) |
| `clean` | drop stale network data of a stopped node |
| `camouflage` | find real sites next to this server that the node can pretend to be |
| `version` | print the version |

### `status` and `wallet`

| Flag | What it does |
|---|---|
| `--json` | print the raw JSON |
| `--quiet` | print nothing; the exit code tells whether the node answers (for health checks) |
| `--status-addr` string | status endpoint of the running node (default "127.0.0.1:7070") |

### `invite`

| Flag | What it does |
|---|---|
| `--app` | print only the echofog:// link, not its https form |
| `--list` | list the peers who joined with the link |
| `--new-secret` | print a fresh access secret for --access-secret / ECHOFOG_ACCESS_SECRET (needs no running node) |
| `--remove` string | take this peer ID off the list of those who joined |
| `--rotate` | make a new access secret: the old link lets nobody new in, those who joined stay |
| `--status-addr` string | status endpoint of the running node (default "127.0.0.1:7070") |

### `pass`

`echofog-node pass <list|new|show|extend|revoke|delete> [ID] [flags]`. Passes are personal invitation
links of a hidden or isolated node, each with its own term, number of devices and, if you want, a
traffic allowance. Make one when somebody pays (or for a friend), extend it when they pay again,
revoke it to take them off. A bot or a website does the same over the operator API:
[operator-api.md](operator-api.md).

| Flag | What it does |
|---|---|
| `--label` string | your note on the pass: a name, an order number |
| `--days` number | term in days (new: 0 = never ends; extend: days added) |
| `--devices` int | how many devices may use it (new: 0 = 2) |
| `--gb` number | traffic through the exit, in GB (new: 0 = no limit; extend: GB added) |
| `--reset` | extend: count the traffic from zero again (a monthly allowance) |
| `--status-addr` string | status endpoint of the running node (default "127.0.0.1:7070") |

### `camouflage`

Looks for real websites next to this server that the node can pretend to be: sites with a publicly
trusted certificate for a real domain that answer TLS 1.3 exactly like the node does. It probes the
server's own /24 first, then the /24s next to it, a few addresses at a time, and prints what it
finds with the two lines for `deploy/.env`. A public node runs the same search by itself; use this
when its log says it found no site, or to choose one yourself. With Docker:
`docker compose run --rm node camouflage` in `deploy/`.

| Flag | What it does |
|---|---|
| `--ip` string | this server's public IPv4 address (default: ECHOFOG_PUBLIC_ADDR, the interface address, or what STUN sees) |
| `--max` int | stop after this many suitable sites (default 5) |

### `init`

| Flag | What it does |
|---|---|
| `--data-dir` string | where the database and identity live (default "/data") |
| `--password` string | password that encrypts the identity (prefer --password-file; without both one is generated and kept in the data directory) |
| `--password-file` string | file holding the password (for example a Docker secret) |

### `restore`

| Flag | What it does |
|---|---|
| `--data-dir` string | where the database and identity live (default "/data") |
| `--force` | replace an identity that already exists in the data directory |
| `--mnemonic` string | the recovery phrase (visible in the process list: prefer --mnemonic-file or the prompt) |
| `--mnemonic-file` string | file holding the recovery phrase |
| `--passphrase` string | the optional extra passphrase, if the phrase was made with one |
| `--password` string | password that encrypts the identity (prefer --password-file; without both one is generated and kept in the data directory) |
| `--password-file` string | file holding the password (for example a Docker secret) |

### `clean`

The node must be stopped: the database opens in one process only. The identity, the TON wallet, the
exit's ledger, the access secret and the list of who joined a hidden node are never touched.

| Flag | What it does |
|---|---|
| `--bans` | also forget the bans of misbehaving peers |
| `--data-dir` string | where the database and identity live (default "/data") |
| `--dry-run` | only count what would be dropped |
| `--mail` | also drop the mail kept for people who are offline (it is lost for them) |
| `--password` string | password that encrypts the identity (prefer --password-file; without both one is generated and kept in the data directory) |
| `--password-file` string | file holding the password (for example a Docker secret) |

## `run`: every flag

### Identity and data

| Flag | Environment variable | What it does |
|---|---|---|
| `--access` string | `ECHOFOG_ACCESS` | who may reach this node: public (listed, its exit advertised), hidden (uses the network as a client, unlisted, answers only people with its link) or isolated (joins no network at all, people with its link connect to it directly; for a server) (default "public") |
| `--access-secret` string | `ECHOFOG_ACCESS_SECRET` | access secret of a hidden or isolated node, 22 characters of base64url (default: generated on first start and kept in the data directory); the same secret on several nodes lets one link in everywhere |
| `--access-secret-file` string | `ECHOFOG_ACCESS_SECRET_FILE` | file holding the access secret (for example a Docker or Kubernetes secret); used when --access-secret is empty |
| `--data-dir` string | `ECHOFOG_DATA_DIR` | where the database and identity live (default "/data") |
| `--mnemonic` string | `ECHOFOG_MNEMONIC` | on first start, restore this identity instead of creating one (prefer --mnemonic-file) |
| `--mnemonic-file` string | `ECHOFOG_MNEMONIC_FILE` | on first start, restore the identity from the recovery phrase in this file |
| `--no-create` | `ECHOFOG_NO_CREATE` | refuse to start without an identity instead of creating a new one |
| `--password` string | `ECHOFOG_PASSWORD` | password that encrypts the identity (prefer --password-file; without both one is generated and kept in the data directory) |
| `--password-file` string | `ECHOFOG_PASSWORD_FILE` | file holding the password (for example a Docker secret) |
| `--public-addr` string | `ECHOFOG_PUBLIC_ADDR` | comma-separated addresses put into the invitation link (host:port or a ws:// URL), when the observed one is not what people should dial |
| `--role` string | `ECHOFOG_ROLE` | client (chat, calls, file transfer and everything else) or server (a relay or exit on a VPS: network services only; nobody can chat with or call this node, mail for others still passes) (default "client") |

### Network and transports

| Flag | Environment variable | What it does |
|---|---|---|
| `--advertise-port` int | `ECHOFOG_ADVERTISE_PORT` | public port peers use to reach this node when it differs from the listen port (e.g. 443 when a port map publishes host 443 to this node); 0 = the listen port |
| `--bootstrap` string | `ECHOFOG_BOOTSTRAP` | comma-separated host:port/peerid of nodes to join through (host may be a domain name; default: the built-in seed nodes) |
| `--camouflage-addr` string | `ECHOFOG_CAMOUFLAGE_ADDR` | host:port that scanners probing the TCP port are forwarded to |
| `--guard-mode` string | `ECHOFOG_GUARD_MODE` | guard mode of a user's node: auto (default; on for a node that is not a server while --obfs is node; the desktop app also stands it down in a country that does not censor), on or off. On: two or three camouflaged guard servers carry its links across a border, instead of a mesh of its own ([anti-dpi.md](anti-dpi.md)) |
| `--listen-addr` string | `ECHOFOG_LISTEN_ADDR` | P2P listen address (UDP for QUIC; the TCP fallback uses the same port unless --tcp-listen is set) (default ":4242") |
| `--no-hole-punch` | `ECHOFOG_NO_HOLE_PUNCH` | do not try direct connections between peers behind NATs |
| `--no-port-mapping` | `ECHOFOG_NO_PORT_MAPPING` | do not ask the router to forward our port (UPnP / NAT-PMP) |
| `--obfs` string | `ECHOFOG_OBFS` | UDP obfuscation: node (default; keys derive from each node's peer ID) or off |
| `--obfs-password` string | `ECHOFOG_OBFS_PASSWORD` | optional extra secret for a private network (UDP obfuscation); every node of that network must use the same |
| `--stealth-password` string | `ECHOFOG_STEALTH_PASSWORD` | optional extra secret for a private network (TCP camouflage); every node of that network must use the same |
| `--stun-servers` string | `ECHOFOG_STUN_SERVERS` | comma-separated STUN servers (default: a built-in list) |
| `--tcp-listen` string | `ECHOFOG_TCP_LISTEN` | TCP fallback listen address, e.g. :443 (default: the --listen-addr port); 'off' disables it |
| `--tcp-remote-port` int | `ECHOFOG_TCP_REMOTE_PORT` | port this node dials on other nodes for the TCP fallback, when their TCP fallback runs on a fixed port apart from QUIC (e.g. 443); 0 = the same port as their QUIC. Not for a port map of your own: that is --advertise-port |
| `--tcp-rotate` string | `ECHOFOG_TCP_ROTATE` | rotate the TCP fallback links against the TSPU freeze (a link that goes silent after ~16 KB): auto (default; across the border from Russia, and where a link froze), on or off |
| `--tls-cert` string | `ECHOFOG_TLS_CERT` | PEM certificate for --tls-server-name (without one a self-signed certificate is used) |
| `--tls-key` string | `ECHOFOG_TLS_KEY` | PEM private key for --tls-cert |
| `--tls-server-name` string | `ECHOFOG_TLS_SERVER_NAME` | domain the TCP fallback presents; use your own domain together with --tls-cert |
| `--ton-network` string | `ECHOFOG_TON_NETWORK` | TON network of the node's wallet: testnet or mainnet (default: testnet) |
| `--transport` string | `ECHOFOG_TRANSPORT` | auto (QUIC, then TCP), quic, or tcp |
| `--ws-client-ip-header` string | `ECHOFOG_WS_CLIENT_IP_HEADER` | header the CDN in front of --ws-listen puts the client address in (CF-Connecting-IP for Cloudflare), so limits count WebSocket peers by their own address; only when the listener is reachable through the CDN alone |
| `--ws-listen` string | `ECHOFOG_WS_LISTEN` | listen for peers over WebSocket on this address, to sit behind a CDN like Cloudflare (e.g. 127.0.0.1:8080 with the CDN in front); empty disables it |
| `--ws-path` string | `ECHOFOG_WS_PATH` | HTTP path the WebSocket endpoint answers on (default /echofog) |

### Relay

| Flag | Environment variable | What it does |
|---|---|---|
| `--bwprobe-mb-per-min` int | `ECHOFOG_BWPROBE_MB_PER_MIN` | most MB a minute this node sends answering speed probes, all peers together (0 = 48) |
| `--no-bwprobe-serve` | `ECHOFOG_NO_BWPROBE_SERVE` | do not answer other peers' download speed probes (12 MB bursts) |
| `--relay` | `ECHOFOG_RELAY` | relay calls and messages for peers that cannot reach each other directly, and keep messages for offline peers (default true) |
| `--relay-allow` string | `ECHOFOG_RELAY_ALLOW` | comma-separated peer IDs allowed to use this relay when it is private |
| `--relay-circuit-mb` int | `ECHOFOG_RELAY_CIRCUIT_MB` | most MB one relayed connection may carry (0 = default 4096, -1 = no limit) |
| `--relay-circuits-per-ip` int | `ECHOFOG_RELAY_CIRCUITS_PER_IP` | most relayed connections the peers of one IP address may hold; a /24 gets twice that (0 = default 32, -1 = no limit) |
| `--relay-circuits-per-peer` int | `ECHOFOG_RELAY_CIRCUITS_PER_PEER` | most relayed connections one peer may hold (0 = default 16, -1 = no limit) |
| `--relay-idle` duration | `ECHOFOG_RELAY_IDLE` | a relayed connection that moved less than --relay-idle-kb in this time is closed (0 = never) (default 5m0s) |
| `--relay-idle-kb` int | `ECHOFOG_RELAY_IDLE_KB` | see --relay-idle (0 = default 32) |
| `--relay-max-circuits` int | `ECHOFOG_RELAY_MAX_CIRCUITS` | most relayed connections at once (only a positive number counts; default 200) |
| `--relay-mbps` int | `ECHOFOG_RELAY_MBPS` | speed of everything relayed through this node in Mbit/s (0 = no limit) |
| `--relay-peer-mb-per-day` int | `ECHOFOG_RELAY_PEER_MB_PER_DAY` | most MB the relayed connections of one peer may carry a day (0 = default 16384, -1 = no limit) |
| `--relay-peer-mbps` int | `ECHOFOG_RELAY_PEER_MBPS` | speed of one peer's relayed connections in Mbit/s (0 = default 50, -1 = no limit) |
| `--relay-private` | `ECHOFOG_RELAY_PRIVATE` | make the relay private: it is not advertised and serves only the peers listed in --relay-allow |
| `--relay-reserved-pct` int | `ECHOFOG_RELAY_RESERVED_PCT` | percent of --relay-max-circuits kept for peers connected 10 minutes or more (0 = default 25, -1 = none) |
| `--relays` string | `ECHOFOG_RELAYS` | comma-separated relays to connect to and prefer, as host:port/<peer id> (use to reach a private relay you were given access to) |
| `--signaling` | `ECHOFOG_SIGNALING` | signaling only: keep the DHT and help peers behind NATs connect directly (hole punching), but relay no traffic and keep no messages |

### Exit (sharing your connection)

| Flag | Environment variable | What it does |
|---|---|---|
| `--exit` | `ECHOFOG_EXIT` | share this connection as an exit node (a public node also publishes it to the DHT; a hidden or isolated one serves only the people with its link) |
| `--exit-free-gb-per-month` int | `ECHOFOG_EXIT_FREE_GB_PER_MONTH` | on a free exit, the GB the whole exit gives away a calendar month (UTC); then it refuses new connections until the month ends (0 = no cap) |
| `--exit-free-ip-mb-per-day` int | `ECHOFOG_EXIT_FREE_IP_MB_PER_DAY` | on a free exit, the MB all the clients of one IP address may take a day (UTC) (0 = no cap) |
| `--exit-free-ip-mbps` int | `ECHOFOG_EXIT_FREE_IP_MBPS` | on a free exit, the speed of all the clients of one IP address in Mbit/s (0 = default 100, -1 = no limit) |
| `--exit-free-kb` int | `ECHOFOG_EXIT_FREE_KB` | free test traffic per client per day in KB (up to 1024, used when the exit is priced; 0 counts as 512 there) (default 512) |
| `--exit-free-mb-per-day` int | `ECHOFOG_EXIT_FREE_MB_PER_DAY` | on a free exit, the MB one client may take a day (UTC) (0 = no cap) |
| `--exit-free-peer-mbps` int | `ECHOFOG_EXIT_FREE_PEER_MBPS` | on a free exit, the speed of one client in Mbit/s (0 = default 50, -1 = no limit) |
| `--exit-free-peers` string | `ECHOFOG_EXIT_FREE_PEERS` | comma-separated peer IDs this exit serves for free, without quota or payment (e.g. your own devices); replaces the list saved in the data directory |
| `--exit-idle` duration | `ECHOFOG_EXIT_IDLE` | close a TCP connection through the exit that moved nothing for this long (0 = never) (default 5m0s) |
| `--exit-max-lifetime` duration | `ECHOFOG_EXIT_MAX_LIFETIME` | close a TCP connection through the exit after this long, busy or not (0 = never) |
| `--exit-max-streams` int | `ECHOFOG_EXIT_MAX_STREAMS` | most connections the exit serves at once (0 = default 4096, -1 = no limit) |
| `--exit-price` string | `ECHOFOG_EXIT_PRICE` | exit price in dollars per GB, paid in TON or USDT, e.g. 0.05 (0 = free) (default "0") |
| `--exit-streams-per-ip` int | `ECHOFOG_EXIT_STREAMS_PER_IP` | most connections the clients of one IP address may have open (0 = default 512, -1 = no limit) |
| `--exit-streams-per-peer` int | `ECHOFOG_EXIT_STREAMS_PER_PEER` | most connections one client may have open through the exit (0 = default 256, -1 = no limit) |
| `--exit-udp-per-peer` int | `ECHOFOG_EXIT_UDP_PER_PEER` | most UDP sessions one client may have open (0 = default 32, -1 = no limit) |
| `--exit-upload-mbps` int | `ECHOFOG_EXIT_UPLOAD_MBPS` | upload speed to advertise in Mbit/s (0 = measure against peers, which needs peers connected first; set it on a server) |
| `--socks5` string | `ECHOFOG_SOCKS5` | local SOCKS5 proxy address used with --use-exit (default "127.0.0.1:1080") |
| `--use-exit` string | `ECHOFOG_USE_EXIT` | route the local SOCKS5 proxy through an exit: 'auto', a peer ID or an invitation link (echofog://join?...) |

### Operator API

| Flag | Environment variable | What it does |
|---|---|---|
| `--api-addr` string | `ECHOFOG_API_ADDR` | address of the operator API (passes for your own payment or moderation, see [operator-api.md](operator-api.md)); '' = off. Needs --api-token |
| `--api-token` string | `ECHOFOG_API_TOKEN` | token the operator API requires ("Authorization: Bearer <token>"), at least 16 characters; a shorter one leaves the API off |

### Limits on peers

| Flag | Environment variable | What it does |
|---|---|---|
| `--max-connections` int | `ECHOFOG_MAX_CONNECTIONS` | how many peers the node dials by itself (only a positive number counts; default: 100) |
| `--max-ids-per-ip` int | `ECHOFOG_MAX_IDS_PER_IP` | most different peer IDs one address may connect with in 10 minutes (0 = default: 32; -1 = no limit) |
| `--max-ids-per-subnet` int | `ECHOFOG_MAX_IDS_PER_SUBNET` | the same per /24 or IPv6 /56 (0 = default: 128; -1 = no limit) |
| `--max-peers` int | `ECHOFOG_MAX_PEERS` | most peers connected to this node by their own dial (0 = default: 256, 2048 on a server; -1 = no limit) |
| `--max-peers-per-ip` int | `ECHOFOG_MAX_PEERS_PER_IP` | most peers connected from one address at once (0 = default: 16; -1 = no limit) |
| `--max-peers-per-subnet` int | `ECHOFOG_MAX_PEERS_PER_SUBNET` | most peers connected from one /24 or IPv6 /56 at once (0 = default: 64; -1 = no limit) |

### Status and logs

| Flag | Environment variable | What it does |
|---|---|---|
| `--log-level` string | `ECHOFOG_LOG_LEVEL` | debug, info, warn or error (default "info") |
| `--stats-every` duration | `ECHOFOG_STATS_EVERY` | how often to log a line with peers, relayed traffic and earnings (0 = never) (default 1m0s) |
| `--status-addr` string | `ECHOFOG_STATUS_ADDR` | local address of the status endpoint used by "status" and "wallet" ('' disables it; keep it on loopback) (default "127.0.0.1:7070") |

