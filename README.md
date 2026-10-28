# EchoFog

Русская версия: [README.ru.md](README.ru.md)

**A peer-to-peer VPN that is hard to block.** There is no company in the middle: you go online
through other people's computers, and anyone can run a node and set their own price. The connection
is disguised as an ordinary browser visiting an ordinary HTTPS site, so a censor's deep-packet
inspection has little to single out.

<p align="center"><a href="https://github.com/AdmiralCore/echofog/releases/download/v0.1.0/demo.mp4"><img src="https://github.com/AdmiralCore/echofog/releases/download/v0.1.0/demo.gif" alt="EchoFog desktop: connecting through a P2P exit, an end-to-end encrypted chat with a 4K60 call, and the TON wallet" width="960"></a></p>

As a bonus, EchoFog includes free end-to-end encrypted chat and group calls with screen sharing up
to 4K60 — no subscription, no central server. Payments for traffic are in TON, with no account and
no commission.

> **Status: experimental.** EchoFog works end to end in the lab and in a Dockerised test network,
> but it has **not** had an independent security audit by a specialist organization yet, and it has
> **not** yet been tested against a real national DPI. Do not rely on it to protect you in a
> high-risk situation yet. Testing and feedback — especially from inside censored networks — are
> exactly what the project needs now. See [SECURITY.md](SECURITY.md) for what is and is not
> protected.

## Documentation

| If you want to… | Read |
|---|---|
| use the desktop app: install, VPN, contacts, calls | [Getting started](docs/getting-started.md) |
| get quick, honest answers (safety, logs, legal risk of sharing) | [FAQ](docs/faq.md) |
| run a node on a server or at home | [Run a node](docs/run-a-node.md) |
| put a node on a VPS with Docker, step by step | [VPS with Docker](docs/deploy-vps.md) |
| protect a public node from floods, fake identities and abuse | [Protecting a node](docs/node-security.md) |
| run an exit only for the people you invite | [Hidden and isolated nodes](docs/hidden-nodes.md) |
| take payment your own way (a bot, a site) and give each person a link with a term | [Operator API](docs/operator-api.md) |
| fix "this computer cannot be reached from the Internet" | [Reachability](docs/reachability.md) |
| understand how it works under the hood | [How it works](docs/how-it-works.md) |
| configure the anti-DPI disguise of a public node | [Anti-DPI](docs/anti-dpi.md) |
| understand paying for traffic with a TON vault | [Vault payments](docs/vault-payments.md) |
| send files to contacts and groups | [File transfer](docs/file-transfer.md) |
| understand how group messages are encrypted and caught up on | [Group encryption](docs/group-encryption.md) |
| know the security status, or report a vulnerability | [SECURITY.md](SECURITY.md) |

Every document has a Russian version next to it (`*.ru.md`).

## How it works, in short

- **No central server.** Nodes find each other through a DHT. They connect directly when their
  routers allow it, punch through home routers with the help of signaling nodes, and fall back to a
  relay when nothing else works.
- **Hard to block.** To a censor, the connection to an exit looks like Chrome opening a real site
  next to the exit's server: HTTP/3 over QUIC, or HTTPS over TCP when UDP is blocked. The app tries
  both at once and keeps whichever gets through. A censor that connects to the node himself, without
  the secret token, lands on that real site. A node can also sit behind a CDN over WebSocket.
- **The VPN.** A local SOCKS5 proxy for apps, or the whole computer through a tunnel (on Windows
  the Wintun driver is built in). Exits compete on an open market; the app can pick one
  automatically by ping, price, speed and which of your sites open through it. An exit can also
  stay hidden and serve only the people it gives an invitation link to.
- **Messages and calls.** End-to-end encrypted (X3DH + Double Ratchet). Replies, threads, edits,
  reactions and pins work in every chat, and a chat looks the same on both phones after a break.
  A group can be a space with text channels and voice rooms, like a community server without the
  server. Pictures arrive with a small preview, so you see them before downloading. Calls and screen
  sharing travel over relay trees sized by each participant's upload, which is how a stream reaches
  many viewers without a central server.
- **Payments.** You keep TON in your own small vault contract and sign vouchers as you use an
  exit; the exit collects them every few hours. Paying by a direct TON or USDT transfer also works.

The longer version is in [How it works](docs/how-it-works.md).

## What a node defends itself against

| Threat | What the node does |
|---|---|
| DPI recognising the protocol | Chrome-shaped QUIC and TLS with the name of a real neighbouring site; the first kilobytes of every tunnel padded so the browser's own handshake inside does not show; connections rotated so no single one runs for an hour carrying gigabytes ([Anti-DPI](docs/anti-dpi.md)) |
| A censor probing the node | without the token, a probe reaches the real site and sees its certificate and pages; an ordinary node stays silent; a replayed, altered or copied handshake counts as a probe |
| UDP blocked or throttled | QUIC and TCP are dialed together; a QUIC link that connects and then goes silent is moved to TCP; the winner is remembered per network, across restarts |
| Connection floods and free fake identities | caps per address and per subnet, a QUIC Retry and a small proof of work for strangers, bans that survive a restart |
| A small request that makes the node send a lot | node-wide budgets for speed probes and peer lists |
| Filling its disk or memory | quotas and expiry for offline mail, DHT records and handed-out keys |
| One user taking the whole relay or exit | per-peer and per-address limits; a free exit gives each client up to 50 Mbit/s by default |
| Fake payments keeping it busy | unknown vaults checked at most every 10 s per peer |
| Being found at all | hidden and isolated nodes stay out of the DHT and answer only invitation links ([Hidden nodes](docs/hidden-nodes.md)) |

What no node can stop by itself: a flood bigger than its uplink, an attacker with thousands of real
addresses, and a censor who crawls the DHT for the addresses of public nodes. The traffic between
nodes (as opposed to client to exit) still uses the older random-looking obfuscation, which a
GFW-style rule can flag. Details: [Protecting a node](docs/node-security.md), [SECURITY.md](SECURITY.md).

## Measured in the lab

From the Docker test network (`echofog-core-oss/testbed`), three runs in a row on 27 October 2026.
A program playing the censor read the traffic at every border while three clients used the VPN for
three minutes (about 2 GB) and a browser visited the imitated site for comparison. This is a lab,
not a real national DPI.

| Measurement | Result |
|---|---|
| EchoFog bytes a GFW-like rule set would flag | 0.0 / 0.0 / 0.1% (on 25 October, before the QUIC disguise: 100%) |
| VPN tunnels to the exit read as ordinary HTTP/3 or HTTPS | all; 5 small mesh links to the exit and 15 to other nodes still look like noise |
| VPN moved to another exit during a run | never |
| HTTPS visits through the VPN given away by the inner handshake | 0 of 24 |
| First page through the VPN with UDP dropped at the border | 0.7 s (before the QUIC/TCP race: 37 s) |
| 300 MB in one stream while connections rotate; 72 downloads at once | whole; 72 of 72 |
| One stream over loopback, plain QUIC / disguised / random-looking | 40 / 37 / 31 MB/s |
| One client through a free exit | 50 Mbit/s, the exit's default per-client limit |

How each number is taken, and what is still missing: [Anti-DPI](docs/anti-dpi.md#measurements).

## Repository layout

This is a Go workspace (`go.work`) with two modules:

| Folder | Module | What it is |
|---|---|---|
| [`echofog-core-oss/`](echofog-core-oss/) | `github.com/echofog/echofog-core` | the node: transport, crypto, NAT traversal, DHT, messaging, calls, proxy, payments, and the headless `echofog-node` binary for servers |
| [`echofog-desktop-oss/`](echofog-desktop-oss/) | `github.com/echofog/echofog-desktop` | the desktop app (Wails: Go + WebView, Svelte front-end); the node runs inside it |

The desktop module uses the core through a `replace` directive pointing at `../echofog-core-oss`,
so the two folders must stay side by side.

## Build from source

You need **Go 1.27.3+**. For the desktop app you also need **Node 18+** and the
[Wails CLI](https://wails.io).

Headless node:

```bash
cd echofog-core-oss
make build                       # -> bin/echofog-node
./bin/echofog-node init          # create an identity, print the recovery phrase
./bin/echofog-node run --bootstrap host:port/<peer id>
```

What the flags mean, which ports to open and how to run it in Docker are in
[Run a node](docs/run-a-node.md).

Desktop app:

```bash
cd echofog-desktop-oss
wails build                      # -> build/bin/EchoFog(.exe)
wails dev                        # live reload against the real core
```

> The Wails v2.11.0 CLI must be built with a recent `golang.org/x/tools` to work under Go 1.27
> (otherwise it fails with `package "context" without types`). Install it with
> `GOTOOLCHAIN=go1.27.3 go install github.com/wailsapp/wails/v2/cmd/wails@v2.11.0` on a machine
> whose module cache already has `x/tools` ≥ v0.52.

Tests:

```bash
cd echofog-core-oss
make test                        # unit and integration tests
make test-calls                  # a 10-node call cluster over QUIC
make testbed-chaos               # ten nodes behind NATs in Docker, with crashes and cuts
```

## Contributing and feedback

Bug reports, test results from censored networks and pull requests are welcome.

- **A bug, or a test from a censored network** (whether it works or not): open an [issue](https://github.com/AdmiralCore/echofog/issues/new/choose) and pick the form.
- **A question or an idea**: start a [discussion](https://github.com/AdmiralCore/echofog/discussions). Vote with 👍 for the ideas you want.
- **Chat and quick help**, mostly in Russian: the [community group](https://t.me/+3N3EKWXi-ow3MzBi) on Telegram. News: the [Telegram channel](https://t.me/EchoFogDev).
- **A security problem**: report it privately, see [SECURITY.md](SECURITY.md).

## License

EchoFog is licensed under the [Mozilla Public License 2.0](LICENSE) (MPL-2.0): changes to
EchoFog's own files must stay open under the same license when you distribute them, while the code
can be combined with code under other licenses. Third-party
code keeps its own licenses — see
[`echofog-core-oss/THIRD_PARTY_NOTICES.md`](echofog-core-oss/THIRD_PARTY_NOTICES.md). The embedded
Wintun driver is redistributed unmodified from [wintun.net](https://www.wintun.net) under its own
terms.
