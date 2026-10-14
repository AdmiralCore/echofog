# EchoFog desktop

Desktop client for [EchoFog](../echofog-core-oss): end-to-end encrypted chats and groups, calls with
screen sharing up to 4K60, and a peer-to-peer proxy market paid in TON or USDT. The node runs inside
the app; there is no server.

Built with [Wails](https://wails.io) (Go + WebView) and Svelte.

## Screens

- **Chats**: contacts (added by their public key), groups and channels (joined by invite), calls.
- **Proxy**: pick an exit and get a local SOCKS5 proxy or route the whole computer; pay the exit
  automatically in small chunks. Or share your own connection and get paid.
- **Wallet**: built-in TON wallet for traffic payments only.
- **Settings**: identity, network options, emergency lock.

## Build

Requirements: Go 1.27+, Node 18+, the Wails CLI (`go install github.com/wailsapp/wails/v2/cmd/wails@v2.11.0`).
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
frontend/src/lib/views      Auth, Shell, Chats, Conversation, Call, Proxy, Wallet, Settings
frontend/src/lib/stores     session, chats, call state
frontend/src/lib/services   call.service.js: WebRTC loopback to the Go media bridge
```

## Third-party assets

UI sounds are synthesized in the app (`frontend/src/lib/sounds.js`), no sound files.

## License

GNU Affero General Public License v3.0 (AGPL-3.0), see [LICENSE](LICENSE). The core it builds on is
under the same license.
