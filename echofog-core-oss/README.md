# EchoFog core

The node software behind EchoFog: free chat, voice/video calls and screen streaming (up to 4K60)
over peer-to-peer relay trees, plus an open market where anyone can sell their Internet connection
as a proxy/VPN exit and get paid in TON or USDT.

There is no central server. Peers find each other through a DHT, connect directly when NATs allow
it and through relays when they do not, and pay each other directly.

## What is in here

| Area | Where | Notes |
|---|---|---|
| Transport | `internal/network` | QUIC with per-node UDP obfuscation (no shared key); TCP fallback that looks like a browser talking TLS to a real site (uTLS Chrome fingerprint, own domain and certificate, smux streams). See [anti-censorship-transport](docs/anti-censorship-transport.md) |
| NAT traversal | `internal/network/{natcheck,portmap}`, `internal/p2p/holepunch` | multi-STUN NAT classification, UPnP/NAT-PMP, coordinated hole punching. See [nat-traversal](docs/nat-traversal.md) |
| Discovery | `internal/p2p/dht`, `internal/services/discovery` | signed records, mDNS on the local network |
| Messaging | `internal/services/{messenger,contacts,groups}` | end-to-end encrypted, double ratchet; delivery receipts and an outbox for offline peers; real group protocol (roles, kick/ban, calls). See [messaging](docs/messaging.md) |
| Calls | `internal/services/call` | relay trees sized by each peer's capacity, so a call scales past what one uplink can carry |
| Proxy / VPN | `internal/network/{exit_gateway,tunmode,fakedns}`, `internal/p2p/vpnmarket` | SOCKS5 and whole-computer (TUN) modes, TCP and UDP |
| Payments | `internal/wallet/tonwallet`, `pkg/api/node/exitpay.go` | prepaid chunks in TON or USDT, no account. See [traffic-payments](docs/traffic-payments.md) |
| Embedding API | `pkg/api/node` | what a GUI or another program calls |
| Node binary | `cmd/echofog-node` | headless node for servers |

## Build

Go 1.27 or newer.

```bash
make build            # bin/echofog-node
make test             # unit and integration tests
make test-calls       # 10-node call cluster over QUIC
make test-calls-tcp   # the same over the TCP fallback
make test-race        # race detector, in Docker
make lint vuln fuzz   # static analysis, known vulnerabilities, fuzzing
make testbed-chaos    # ten nodes behind NATs in Docker, with crashes and cuts
```

See [docs/testing.md](docs/testing.md) for what each layer is for.

## Run a node

```bash
bin/echofog-node init                               # new identity: prints the recovery phrase once
bin/echofog-node restore                            # or restore one (asks for the phrase, hidden)
bin/echofog-node run --data-dir ./data --bootstrap seed1.example.org:4242/<peer id>
bin/echofog-node status                             # peers, relayed traffic, earnings, wallet
```

`run` without an identity creates one (or restores it with `--mnemonic-file`), and keeps a
generated password in `<data-dir>/.node_password` unless `--password-file` is given. Peers are named
by `host:port/<peer id>`, the host may be a domain: packets to a node are obfuscated with a key
derived from its peer ID, so the ID is needed to talk to it. A node prints its own `Join address`.
Every flag is also an `ECHOFOG_*` environment variable; `echofog-node run -h` lists them.

- **Relay** (on by default, `--relay-max-circuits`): calls and messages between peers that cannot
  reach each other directly, and a mailbox for offline peers.
- **Signaling only** (`--signaling`): the DHT and help with hole punching for peers behind NATs,
  no relayed traffic; cheap in bandwidth.
- **Exit**: `--exit --exit-price 0.05` sells the connection, dollars per GB, paid in TON or USDT
  into the node's own wallet; the node books incoming payments by itself.
- **Use an exit**: `--use-exit auto --socks5 127.0.0.1:1080`, then point a browser or any
  SOCKS5-aware program at `127.0.0.1:1080`.
- **Logs**: a stats line every minute (`--stats-every`) with peers, relayed traffic and messages,
  exit sessions, earnings and the wallet balance.

Public nodes with Docker and Google Cloud, and why not behind the Cloudflare proxy:
[docs/deploy.md](docs/deploy.md).

## Layout

```
cmd/echofog-node      headless node
pkg/node              the node: identity, network, services
pkg/api/node          API for embedding applications
pkg/api/types         public types
pkg/dto, pkg/events   data objects and the event bus
internal/...          everything else (not importable from outside)
docs/                 design notes
```

## Status

Pre-release. The test suite covers the protocol, call trees, payments (with a mocked chain) and a
10-node cluster; real-world NAT behaviour and live TON payments still need testing on the real
network. Do not put more into a node's wallet than you are ready to lose.

## Third-party code

See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## License

GNU Affero General Public License v3.0 (AGPL-3.0), see [LICENSE](LICENSE). Anyone who runs a modified
version as a network service has to offer its source to the users of that service. Third-party code
keeps its own licenses (see THIRD_PARTY_NOTICES.md where present).
