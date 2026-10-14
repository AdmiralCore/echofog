# EchoFog

A peer-to-peer messenger with a built-in proxy market, designed to keep working where the network
is censored. Free end-to-end encrypted chat, voice/video calls and screen sharing; plus an open
market where anyone can sell their Internet connection as a proxy/VPN exit and get paid in crypto.

There is no central server. Peers find each other through a DHT, connect directly when their NATs
allow it and through relays when they do not, and pay each other directly. The transport tries to
look like an ordinary browser visiting a big HTTPS site, so a censor's deep-packet inspection sees
nothing it can single out.

> **Status: experimental.** The protocol works end to end in the lab and in a Dockerised test
> network, but it has **not** had an external security audit and has **not** been tested against a
> real national DPI. Do not rely on it to protect you in a high-risk setting yet. Feedback and
> testing — especially from inside censored networks — are exactly what the project needs now.

## Repository layout

This is a Go workspace (`go.work`) with two modules:

| Folder | Module | What it is |
|---|---|---|
| [`echofog-core-oss/`](echofog-core-oss/) | `github.com/echofog/echofog-core` | the node: transport, crypto, NAT traversal, DHT, messaging, calls, proxy, payments, and the headless `echofog-node` binary for servers |
| [`echofog-desktop-oss/`](echofog-desktop-oss/) | `github.com/echofog/echofog-desktop` | the desktop app (Wails: Go + WebView, Svelte front-end). The node runs inside it. |

The desktop module uses the core through a `replace` directive pointing at `../echofog-core-oss`,
so the two folders must stay side by side.

## How it works

The short version (detailed public docs are being prepared):

- **Transport and anti-censorship.** QUIC first, with every UDP packet obfuscated under a key
  derived from the peer's ID (no key is shared by the whole network, and none is in the binary).
  When UDP is blocked, the node falls back to TCP that looks like a real browser's HTTPS session:
  a genuine Chrome TLS ClientHello (uTLS), a REALITY-style token hidden in the ClientHello, and
  anyone who connects without a valid token is relayed to the real mimicked site and sees *its*
  certificate. A WebSocket transport lets a node sit behind a CDN such as Cloudflare. See
  anti-censorship-transport.md and the
  nDPI classification report.
- **NAT traversal.** Multi-STUN NAT classification, UPnP/NAT-PMP port mapping and coordinated hole
  punching; a relay is used when a direct path is impossible. See
  nat-traversal.md.
- **Messaging.** End-to-end encrypted (X3DH + Double Ratchet), with delivery receipts, an outbox
  for offline peers, and a real group protocol. See
  messaging.md.
- **Calls.** Media flows over relay trees sized by each peer's uplink, so a call scales past what
  one connection could carry.
- **Proxy / VPN.** A local SOCKS5 proxy or a whole-computer tunnel (TUN), over an exit someone
  sells on the market. On Windows the TUN driver (Wintun) is embedded and written out on demand, so
  there is nothing extra to install.
- **Payments.** Prepaid in small chunks, in TON or USDT, with no account. See
  traffic-payments.md.

Two kinds of public node matter for a healthy network: **relays**, which carry traffic for peers
that cannot reach each other directly, and **signaling nodes** (`--signaling`), which only keep the
DHT and help with hole punching and so cost almost no bandwidth.

A relay can be **public** (the default — it serves anyone) or **private** (`--relay-private`): a
private relay is not advertised and serves only the peer IDs its operator lists in `--relay-allow`,
so you can run one on your own VPS for yourself and a few friends. Those friends reach it by adding
its address with `--relays host:port/<peer id>` (and are added for free with their peer ID on the
relay side); a client dials and prefers the relays named there even though a private one does not
advertise itself.

## Quick start

You need **Go 1.27+**. For the desktop app you also need **Node 18+** and the
[Wails CLI](https://wails.io).

### Run a headless node

```bash
cd echofog-core-oss
make build                       # -> bin/echofog-node
./bin/echofog-node init          # create an identity, print the recovery phrase
./bin/echofog-node run           # join and serve
```

Running real public nodes (seeds, relays, exits) on a server, with Docker and on Google Cloud, is
covered step by step in deploy.md.

### Build the desktop app

```bash
cd echofog-desktop-oss
wails build                      # -> build/bin/EchoFog(.exe)
wails dev                        # live reload against the real core
```

> Note: the Wails v2.11.0 CLI must be built with a recent `golang.org/x/tools` to work under
> Go 1.27 (otherwise it fails with `package "context" without types`). Install it with
> `GOTOOLCHAIN=go1.27.2 go install github.com/wailsapp/wails/v2/cmd/wails@v2.11.0` on a machine
> whose module cache already has `x/tools` ≥ v0.52.

### Test

```bash
cd echofog-core-oss
make test                        # unit and integration tests
make test-calls                  # a 10-node call cluster over QUIC
make testbed-chaos               # ten nodes behind NATs in Docker, with crashes and cuts
```

See testing.md for what each layer covers.

## Security model, honestly

- Message content is end-to-end encrypted; the node you relay through sees only ciphertext.
- The transport hides the *protocol* from a passive observer, and makes an active prober see a real
  website — but it does **not** hide the *existence* of a public node from a censor who crawls the
  DHT. Hiding those needs unlisted bridges (nodes whose address is shared privately).
- What is **not** done yet: an external audit; testing against a real national DPI; the settlement
  side of relay/exit payments. Treat prices as advertised, not enforced.

If you find a weakness, please open an issue or reach the maintainers before disclosing it widely.

## License

Each module carries its own `LICENSE` file; see
[`echofog-core-oss/LICENSE`](echofog-core-oss/LICENSE) and
[`echofog-desktop-oss/LICENSE`](echofog-desktop-oss/LICENSE). The embedded Wintun driver is
redistributed unmodified from [wintun.net](https://www.wintun.net) under its own terms.
