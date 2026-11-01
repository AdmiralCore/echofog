# EchoFog core

The node software behind EchoFog: free chat, voice/video calls and screen streaming (up to 4K60)
over peer-to-peer relay trees, plus an open market where anyone can sell their Internet connection
as a proxy/VPN exit and get paid in TON or USDT.

There is no central server. Peers find each other through a DHT, connect directly when NATs allow
it and through relays when they do not, and pay each other directly.

## What is in here

| Area | Where | Notes |
|---|---|---|
| Transport | `internal/network` | QUIC to a camouflage node shaped like Chrome's HTTP/3 to a real neighbouring site (token in the ClientHello random, strangers forwarded to the site; apernet/quic-go fork in `third_party/quic-go`); per-node UDP obfuscation between other nodes (no shared key); TCP fallback that looks like Chrome over TLS to the same site (uTLS, smux streams); TCP rotation against Russia's 16 KB freeze (`rotconn.go`, `tcp_rotate.go`); WebSocket for a node behind a CDN (`wstransport.go`); dialing through SOCKS5 or Tor (`proxy.go`); the dial allow-list of anonymous mode (`dialonly.go`); QUIC and TCP dialed together with per-network memory; guard mode in `internal/p2p/swarm` (`swarm_guards.go`). See [anti-dpi](../docs/anti-dpi.md) |
| NAT traversal | `internal/network/{natcheck,portmap}`, `internal/p2p/holepunch` | multi-STUN NAT classification, UPnP/NAT-PMP/PCP, coordinated hole punching. See [reachability](../docs/reachability.md) |
| Discovery | `internal/p2p/dht`, `internal/services/discovery` | signed records, mDNS on the local network |
| Messaging | `internal/services/{messenger,contacts,groups}` | end-to-end encrypted, double ratchet; delivery receipts and an outbox for offline peers; real group protocol (roles, kick/ban, calls); mailbox relays that keep mail for an offline peer (`internal/p2p/swarm/swarm_mailbox.go`). See [how it works](../docs/how-it-works.md#messages) |
| Groups and spaces | `internal/services/convlog`, `pkg/api/node` | the group's event log: channels and voice rooms, invitations, per-channel overrides, timeouts, the moderation log, roles, the group photo; join requests and the group file (`.fogzip`). See [groups](../docs/groups.md) |
| Files | `internal/services/filexfer` | files to contacts and groups, resumable. See [file transfer](../docs/file-transfer.md) |
| Calls | `internal/services/call` | relay trees sized by each peer's capacity, so a call scales past what one uplink can carry |
| Proxy / VPN | `internal/network/{exit_gateway,tunmode,fakedns}`, `internal/p2p/vpnmarket` | SOCKS5 and whole-computer (TUN) modes, TCP and UDP; a pool of up to three rotating connections to an exit, and padding of each tunnel's first kilobytes |
| Access | `internal/network/access` | hidden and isolated nodes: the access secret checked in the handshake, hole punch tickets, `echofog://join` invitation links. See [hidden nodes](../docs/hidden-nodes.md) |
| Payments | `internal/wallet/tonwallet`, `pkg/api/node/exitpay.go` | a TON vault with vouchers, or direct TON or USDT transfers, no account. See [vault payments](../docs/vault-payments.md) |
| Embedding API | `pkg/api/node` | what a GUI or another program calls |
| Node binary | `cmd/echofog-node` | headless node for servers |

## Build

Go 1.27.3 or newer.

```bash
make build            # bin/echofog-node
make test             # unit and integration tests
make test-calls       # 10-node call cluster over QUIC
make test-calls-tcp   # the same over the TCP fallback
make test-race        # race detector, in Docker
make lint vuln fuzz   # static analysis, known vulnerabilities, fuzzing
make testbed-chaos    # ten nodes behind NATs in Docker, with crashes and cuts
```

How the traffic looks to a censor is measured with `go run ./testbed/runner -scenario censor`
(see [testbed/README.md](testbed/README.md)).


## Run a node

```bash
bin/echofog-node init                               # new identity: prints the recovery phrase once
bin/echofog-node restore                            # or restore one (asks for the phrase, hidden)
bin/echofog-node run --data-dir ./data --bootstrap seed1.example.org:4242/<peer id>
bin/echofog-node status                             # peers, relayed traffic, earnings, wallet
bin/echofog-node invite                             # invitation link of a hidden or isolated node
bin/echofog-node wallet                             # TON wallet address and balance
bin/echofog-node pass                               # personal links with a term (see -h)
bin/echofog-node clean                              # drop stale DHT records and peers (stopped node)
bin/echofog-node camouflage                         # find real sites next to this server to imitate
bin/echofog-node version
```

`run` without an identity creates one (or restores it with `--mnemonic-file`), and keeps a
generated password in `<data-dir>/.node_password` unless `--password-file` is given. Peers are named
by `host:port/<peer id>`, the host may be a domain: packets to a node are obfuscated with a key
derived from its peer ID, so the ID is needed to talk to it. An address may also carry
`?sni=<site it imitates>&srv=1`, so the first dial to a server is already disguised. A node prints
its own `Join address`. Every flag is also an `ECHOFOG_*` environment variable;
`echofog-node run -h` lists them.

- **Role** (`--role`): `client` (the default for the binary) or `server`. A server is a relay or
  exit only: nobody can chat with or call it, and mail for others still passes. The VPS compose
  file sets `ECHOFOG_ROLE=server` unless `.env` says otherwise.
- **Relay** (on by default, `--relay-max-circuits`): calls and messages between peers that cannot
  reach each other directly, and a mailbox for offline peers.
- **Signaling only** (`--signaling`): the DHT and help with hole punching for peers behind NATs,
  no relayed traffic; cheap in bandwidth.
- **Exit**: `--exit --exit-price 0.05` sells the connection, dollars per GB, paid in TON or USDT
  into the node's own wallet; the node books incoming payments by itself. The exit is public
  (advertised to everybody) unless the node runs with `--access`. A free exit can cap what it
  gives: `--exit-free-mb-per-day` and `--exit-free-ip-mb-per-day` per client and per address,
  `--exit-free-gb-per-month` for the whole exit (all off by default).
- **Anti-DPI**: `--tcp-rotate auto|on|off` (the TCP fallback in short connections against the
  16 KB freeze), `--guard-mode auto|on|off` (a user's node goes across the border only through
  camouflaged guards). See [anti-dpi](../docs/anti-dpi.md).
- **Access**: `--access hidden` keeps the node unlisted and answers only the people with its
  invitation link (`echofog-node invite`), its contacts and those who joined before;
  `--access isolated` also joins no network and only accepts direct connections. See
  [hidden nodes](../docs/hidden-nodes.md).
- **Operator API**: `--api-addr` with `--api-token` (16 characters or more) serves the passes of
  a hidden or isolated node over HTTP. See [operator API](../docs/operator-api.md).
- **Use an exit**: `--use-exit auto --socks5 127.0.0.1:1080` (or a peer ID, or an invitation
  link in quotes), then point a browser or any SOCKS5-aware program at `127.0.0.1:1080`.
- **Logs**: a stats line every minute (`--stats-every`) with peers, relayed traffic and messages,
  exit sessions, earnings and the wallet balance.

Docker: `deploy/docker-compose.yml` with `deploy/.env.example`. Step-by-step guides:
[run a node](../docs/run-a-node.md), [VPS with Docker](../docs/deploy-vps.md),
[hidden and isolated nodes](../docs/hidden-nodes.md).

## Layout

```
cmd/echofog-node      headless node
pkg/node              the node: identity, network, services
pkg/api/node          API for embedding applications
pkg/api/types         public types
pkg/dto, pkg/events   data objects and the event bus
internal/...          everything else (not importable from outside)
```

## Status

Pre-release. The test suite covers the protocol, call trees, payments (with a mocked chain) and a
10-node cluster; real-world NAT behaviour and live TON payments still need testing on the real
network. Do not put more into a node's wallet than you are ready to lose.

## Third-party code

See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## License

Mozilla Public License 2.0 (MPL-2.0), see [LICENSE](../LICENSE) at the repository root. Changes to EchoFog's own files must
stay open under the same license when you distribute them; you may combine EchoFog with code under
other licenses. Third-party code keeps its own licenses (see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)).
