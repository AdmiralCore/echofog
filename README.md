# EchoFog

Русская версия: [README.ru.md](README.ru.md)

**A peer-to-peer messenger for communities that keeps working where others are blocked.** Groups with
channels and voice rooms, calls, screen streams up to 4K at 60 fps, files and offline delivery, with no
server in the middle that anyone can switch off. Messages and calls are end-to-end encrypted. Where the
network is filtered, the connection is disguised as an ordinary visit to an ordinary website.

A VPN is built in as a second feature: go online through an exit that a friend or an operator runs, or
share your own.

> **Status: 0.3.0, the first release on protocol v2. Experimental.** It works end to end in our lab and
> test networks, but it has **not** had an independent security audit, and it has **not** been tested
> inside a real national censorship system yet. Do not rely on it where your safety depends on it.
> Testing and feedback, especially from inside filtered networks, are what the project needs most now.
> What is and is not protected: [SECURITY.md](SECURITY.md).

## What you can do with it

- **Communities without a server.** A group is a space with text channels, categories, threads, roles
  and per-channel permissions, invitations with a term, join requests, slow mode and bans. Its history
  lives on the members' devices, signed by its authors.
- **Talk.** Direct chats with replies, threads, reactions, edits, pins, mentions and saved messages.
  Voice rooms you join with one click, calls one to one and in groups, and screen streams up to 4K60
  that travel over trees of the viewers' own uplinks, so a stream reaches many people without a server.
- **Files and voice messages.** Any size, resumed after a break, delivered into the group, channel and
  thread they were sent to.
- **Offline delivery.** A message to someone who is offline waits in an encrypted mailbox for up to
  7 days. The app shows honest states: *waiting for connection*, *in mailbox*, *delivered*, *expired*.
- **A VPN when you need one.** Paste a link to an exit and go online through it, as a local SOCKS5 proxy
  or for the whole computer with a kill switch (Windows and Linux). Turn your own computer into an exit
  for the people you choose.
- **Run a node.** One command installs a server node with a full-screen setup: presets, a system check,
  autostart, a link and a QR code at the end. A node can relay, keep mailboxes, serve as an exit and
  hold the camouflage site.

## Documentation

| If you want to… | Read |
|---|---|
| install the app, add contacts, create a space, call | [Getting started](docs/getting-started.md) |
| set up a space: channels, roles, invitations, moderation | [Groups and spaces](docs/groups.md) |
| send files to contacts and groups | [File transfer](docs/file-transfer.md) |
| get short, honest answers (safety, logs, legal risk) | [FAQ](docs/faq.md) |
| run a node on a VPS | [VPS with protocol v2](docs/deploy-vps-v2.md), [Node command line](docs/node-cli.md) |
| check that an installer or a release is genuine | [Release trust](docs/release-trust.md) |
| run an entry inside Russia or Iran | [Domestic entry](docs/domestic-entry.md) |
| use the kill switch | [Windows](docs/kill-switch-windows.md), [Linux](docs/kill-switch-linux.md) |
| understand the disguise and what it does not hide | [Anti-DPI](docs/anti-dpi.md) |
| read the protocol | [Protocol v2](docs/protocol/v2/README.md) |
| know the security status, or report a vulnerability | [SECURITY.md](SECURITY.md) |

Every user document has a Russian version next to it (`*.ru.md`). Documents about protocol v1 are being
replaced; where a page still describes v1, it says so at the top.

## How it works, in short

- **No central server.** Nodes find each other through a DHT and peer exchange. They connect directly
  when the routers allow it, punch through home routers (including symmetric NATs), and use another
  node as a relay when nothing else works. A relay sees only ciphertext.
- **End-to-end encryption.** A chat starts with PQXDH, a post-quantum hybrid (X25519 and ML-KEM-768),
  and continues with a double ratchet. Each member of a group gets messages over their own session.
  Calls are encrypted inside these sessions; in 0.3.0 the members who forward a stream to others in a
  call can see it (details in [SECURITY.md](SECURITY.md)).
- **Hard to block.** Across a filtered border, the connection looks like a browser opening the node's
  own website over HTTP/3, or over HTTPS when UDP is blocked. A censor who connects to the node without
  the secret token gets that website. Each country has a route policy: for example, in Russian mobile
  networks the app goes through an entry inside the country, and a connection that freezes after about
  16 KB is marked and avoided. Between peers, where nothing filters, traffic is plain random-looking UDP.
- **Your address.** Hidden nodes stay out of the DHT and answer only invitation links. The app's own
  traffic can go through Tor or another SOCKS5 proxy, and calls can be set to go only through a relay.

The longer version: [How it works](docs/how-it-works.md).

## What we have measured, and what not yet

From our lab, not from a real national censor:

| Measurement | Result |
|---|---|
| Disguised QUIC link (reality2-quic) against the lab's detectors: fingerprint, SNI, SNI vs address, flow shape | 0 flags |
| Active probes of a node (replay, altered handshake, cover check, token) | 0 answers that give the node away |
| Screen stream 1440p60, 1440p144, 2160p60, direct and through a relay, against protocol v1 | same frame rate, 0 % loss |
| Voice in a room of 5, p95 delay | 135 ms (v1: 136 ms) |
| Relay of a voice room cut off without warning | listeners back in 0.7 s |

Not proven yet, and therefore not promised: that the TCP disguise is indistinguishable from a browser
(record sizes and timings still differ), anything about China or Iran, and behaviour in real mobile
networks of Russia. These are the next lab and field tasks.

## Repository layout

A Go workspace (`go.work`) with two modules:

| Folder | Module | What it is |
|---|---|---|
| [`echofog-core-oss/`](echofog-core-oss/) | `github.com/echofog/echofog-core` | the node: transport, crypto, NAT traversal, DHT, messaging, calls, files, VPN, and the `echofog-node` server binary. Protocol v2 lives in `internal/v2/` |
| [`echofog-desktop-oss/`](echofog-desktop-oss/) | `github.com/echofog/echofog-desktop` | the desktop app (Wails: Go + WebView, Svelte front-end); the node runs inside it |

The desktop module uses the core through a `replace` directive pointing at `../echofog-core-oss`, so
the two folders must stay side by side.

## Build from source

You need **Go 1.27.3+**. For the desktop app also **Node 18+** and the [Wails CLI](https://wails.io).

Server node:

```bash
cd echofog-core-oss
make build                         # -> bin/echofog-node
./bin/echofog-node v2 setup        # full-screen installer; with --yes and flags it runs unattended
```

To install a released node on a server, use the signed `install.sh` and check it first:
[VPS with protocol v2](docs/deploy-vps-v2.md).

Desktop app:

```bash
cd echofog-desktop-oss
wails build                        # -> build/bin/EchoFog(.exe)
wails dev                          # live reload against the real core
```

> The Wails v2.11.0 CLI must be built with a recent `golang.org/x/tools` to work under Go 1.27
> (otherwise it fails with `package "context" without types`). Install it with
> `GOTOOLCHAIN=go1.27.3 go install github.com/wailsapp/wails/v2/cmd/wails@v2.11.0` on a machine whose
> module cache already has `x/tools` ≥ v0.52.

Tests:

```bash
cd echofog-core-oss
make test                          # unit and integration tests
make test-race                     # the same under the race detector
make testbed                       # the Docker test network
```

## Contributing and feedback

Bug reports, test results from filtered networks and pull requests are welcome.

- **A bug, or a test from a filtered network** (whether it works or not): open an
  [issue](https://github.com/AdmiralCore/echofog/issues/new/choose) and pick the form.
- **A question or an idea**: start a [discussion](https://github.com/AdmiralCore/echofog/discussions).
- **Chat and quick help**, mostly in Russian: the [community group](https://t.me/+3N3EKWXi-ow3MzBi) on
  Telegram. News: the [Telegram channel](https://t.me/EchoFogDev).
- **A security problem**: report it privately, see [SECURITY.md](SECURITY.md).

## License

EchoFog is licensed under the [Mozilla Public License 2.0](LICENSE) (MPL-2.0): changes to EchoFog's own
files must stay open under the same license when you distribute them, while the code can be combined
with code under other licenses. Third-party code keeps its own licenses, see
[`echofog-core-oss/THIRD_PARTY_NOTICES.md`](echofog-core-oss/THIRD_PARTY_NOTICES.md). The embedded Wintun
driver is redistributed unmodified from [wintun.net](https://www.wintun.net) under its own terms.
