# EchoFog desktop

Desktop client for [EchoFog](../echofog-core-oss): end-to-end encrypted chats and groups, calls with
screen sharing up to 4K60, and a peer-to-peer proxy market paid in TON or USDT. The node runs inside
the app; there is no server.

Built with [Wails](https://wails.io) (Go + WebView) and Svelte.

## Screens

- **Chats**: contacts (added by their public key), groups and channels (joined by invite), calls.
- **Proxy**: pick an exit and get a local SOCKS5 proxy or route the whole computer; pay the exit
  automatically in small chunks. **Specific node** also takes an invitation link (`echofog://join?…`)
  to a hidden exit. Or share your own connection and get paid; the **Access to this computer** card
  makes it Public or Hidden and gives the invitation link (see
  [Hidden and isolated nodes](../docs/hidden-nodes.md)).
- **Wallet**: built-in TON wallet for traffic payments only.
- **Settings**: identity, network options, emergency lock.
- **Logs**: the app's log, with secrets blanked out, to copy or save.

## Build

Requirements: Go 1.27+, Node 18+, the Wails CLI v2.11.0 built with a recent `golang.org/x/tools`
(see [Build from source](../README.md#build-from-source) for why and how).
The core is expected next to this folder (`../echofog-core-oss`, see the `replace` in `go.mod`).

```bash
wails build            # build/bin/EchoFog(.exe)
wails dev              # live reload against the real core
```

### UI without the core

```bash
cd frontend && npm install && npm run dev
```

Open `http://localhost:5173/?mock` (add `&incoming` to get an incoming call). A fake core in
`frontend/src/lib/dev/mock.js` answers every call, including a real WebRTC loopback for calls.

## Layout

```
app*.go                     methods the UI calls (bound by Wails)
frontend/src/App.svelte     sign-in or main window
frontend/src/lib/views      Auth, Shell, Chats, Conversation, Saved, Proxy, Wallet, Settings, Logs
frontend/src/lib/components cards and widgets (AccessCard, RelayCard, call/ for the call overlay)
frontend/src/lib/stores     session, chats, call state
frontend/src/lib/services   call.service.js: WebRTC loopback to the Go media bridge
```

## Third-party assets

UI sounds are synthesized in the app (`frontend/src/lib/sounds.js`), no sound files.

## License

Mozilla Public License 2.0 (MPL-2.0), see [LICENSE](../LICENSE) at the repository root. The core it builds on is under the
same license.
