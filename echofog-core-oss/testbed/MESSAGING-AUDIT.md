# EchoFog messaging audit — messages, calls, streams

Module `github.com/echofog/echofog-core`. This is an end-to-end read of the delivery
paths for text messages (1:1 and group), calls (1:1 and group) and media streams, their
failure modes, and a ranked list of real bugs and gaps. It is paired with the near-real
lab scenario `go run ./testbed/runner -scenario messaging`, which exercises every path
across NATs and relays and prints a top-of-output delivery trace with the failures first.

Date: 2026-10-27. Branch: `exp/reality-quic-sni`.

---

## 1. Text messages (1:1)

### Path (send)

`MessageBroker.sendWithTier` ([internal/services/messenger/message_broker.go:137](../internal/services/messenger/message_broker.go))

1. Build the inner `PersonalMessageProto` (id, sender, text, metadata, timestamp).
2. Encrypt by tier:
   - `TierX3DH` (default): Signal X3DH + Double Ratchet (`crypto.EncryptMessage`). If there
     is **no session and no key bundle yet**, the message is parked as *intent*
     (`trackIntent`), a knock is sent for the peer's keys, a connection is requested, and the
     call returns `StateKeys`. It is encrypted and sent later, when the bundle arrives.
   - `TierChaCha20`: stateless XChaCha20-Poly1305 with a pre-shared 32-byte key (magic `0x01`).
   - `TierPlain`: signed but unencrypted (magic `0x00`).
3. `deliver(target, msg)` ([:277](../internal/services/messenger/message_broker.go)):
   - **Direct**: `Swarm.SendDataForPeer` — succeeds only if the target is an *already
     connected* peer. There is **no multi-hop routing inside the swarm**; `SendDataForPeer`
     returns an error for any peer that is not an active connection.
   - **Else relay**: `sendViaRelay` wraps the signed envelope in a `RelayRequest` and sends it
     (`TypeRelayMessageRequest`) to the **first** connected peer that advertises as a relay (or
     a configured private relay) and is not currently marked full.
4. `track` ([outbox.go:72](../internal/services/messenger/outbox.go)) stores the finished
   ciphertext in the outbox keyed by a blinded target, with `Sent`/`Next`/`Attempts`.

### Path (retry / store-and-forward on the sender)

- `StartOutbox` ([outbox.go:250](../internal/services/messenger/outbox.go)) ticks every 3 s and
  calls `FlushOutbox(zero)` which re-`deliver`s every due item with backoff 2s→5s→15s→45s→60s.
- `OnPeerConnected` ([outbox.go:303](../internal/services/messenger/outbox.go)) flushes that
  peer's queue immediately. The broker is registered as a swarm observer
  ([pkg/node/node.go:573](../pkg/node/node.go)), so delivery resumes the moment the peer (or a
  relay) connects.
- Items past `outboxTTL` (7 days) are dropped with `StateExpired`.

### Path (relay store-and-forward)

`RelayServer.Handle` ([internal/p2p/routing/relay_server.go:663](../internal/p2p/routing/relay_server.go)):

- target == self → unwrap and push to the local dispatcher.
- target directly connected → forward immediately (`TypeChatMessage`).
- `HopLimit > 1` → forward to the next relay (one hop).
- otherwise → `MailboxManager.Store` for the offline recipient.
- On `OnPeerConnected` the relay delivers every pending mailbox item to the returning peer and
  `Acknowledge`s them.

`MailboxManager` ([mailbox.go](../internal/services/messenger/mailbox.go)) enforces per-recipient,
per-sender and node-wide quotas and, crucially, **`RequireRecipientSeen`**: it keeps a message
only for a recipient that has itself used this relay within `RecipientSeenTTL` (24 h). A refusal
(`ErrMailboxFull`, `ErrItemTooLarge`, `ErrRecipientUnknown`) is answered to the sender with
`TypeMailboxFull`; the sender's `RelayClient.MarkRelayFull` blacklists that relay for a while and
the broker skips it on the next attempt ([message_broker.go:322](../internal/services/messenger/message_broker.go)).

### Path (receive + ack)

`MessageBroker.Handle` ([message_broker.go:333](../internal/services/messenger/message_broker.go)):
decrypt by payload type; the authenticated origin (signed envelope), not the claimed `SenderId`,
is trusted; knock / reset / `fogack1:` protocol prefixes are handled inline; **every** message id
is acked (`go sendAck`, even duplicates, so a lost receipt re-acks) and then de-duplicated by
`firstSight` (a `seen/<id>` key). Accepted messages are stored and pushed to callbacks. A reply
`fogack1:<id>` (always X3DH) reaches the sender's `confirm`, which removes the item and emits
`StateDelivered`.

### Message states

`StateKeys` → `StateOffline` → `StateSent` → `StateDelivered`, or `StateExpired`
([outbox.go:32](../internal/services/messenger/outbox.go)). These are surfaced to the UI via
`OnMessageState`, and per-chat via `PendingStates`.

### Failure modes

| Failure | Behaviour | Visible? |
|---|---|---|
| Peer offline, shares a relay the peer has used | queued, stored at relay, delivered on reconnect | pending → delivered |
| Peer offline, shares **no** relay the peer has used | relay refuses (`ErrRecipientUnknown`→`TypeMailboxFull`), sender rotates relays; if none qualify, sits in outbox as "sent/offline" until peer comes online **directly** or TTL | pending only — **no distinct "undeliverable"** |
| Session desync (both start a ratchet at once, or one loses state) | undecryptable → `sessionBroken`/`sessionReset`, both drop the session, outbox re-encrypted with a fresh one | self-healing (see `TestBrokenSessionHeals`) |
| Relay full / item too large | `TypeMailboxFull` → relay blacklisted, another tried | retried |
| Lost receipt | sender retransmits, receiver re-acks | converges |

---

## 2. Text messages (group)

`groupsFacade.post` ([pkg/api/node/groups.go:471](../pkg/api/node/groups.go)) signs the content,
seals it with the group's sender key / epoch for capable members, then **fans out one 1:1 message
per member** via `broadcastMsg` ([pkg/api/node/group_sync.go:194](../pkg/api/node/group_sync.go)),
which calls `Messenger.SendSilent` per recipient. So **group delivery reuses the entire 1:1 path**
(encryption, outbox, relay, mailbox) once per member, and `post` returns `Delivered / Queued /
Failed` counts that are the sender's own accounting of that fan-out. The receiver takes group
traffic through the messenger interceptor and stores it under the group.

### Failure modes

- Same per-member failure matrix as 1:1 (including the "no shared relay" gap below).
- **No history backfill.** Fan-out happens only at send time. A member offline longer than the
  7-day outbox TTL, or who **joins after** a message was posted, never receives that message —
  there is no group message sync on (re)join. (Membership/manifest state does sync; message
  history does not.)
- Large groups: members beyond `meshLimit` (50) are not nudged to interconnect, so fan-out leans
  harder on relays.

---

## 3. Calls (1:1 and group)

### Setup / signaling reach

`InviteToCall` ([pkg/api/node/groups.go:664](../pkg/api/node/groups.go)) sends the invitation as
an ordinary **tracked chat message** `fogcall://<callID>/<coordinator>` via
`SendChatMessageState`. So **"does signaling reach the callee" is exactly "does a message reach
the peer"** — it rides the outbox/relay/mailbox path and is observable through message state and
`/pending`. The callee parses the invite (`ParseCallInvite`) and joins the room.

In-call signaling is a separate, richer protocol in `call.CallManager`
([internal/services/call/manager_signaling.go](../internal/services/call/manager_signaling.go)):
signed `CallSignalEnvelope`s over the room's media dispatcher outbox (`TypeCallSignaling`), a
coordinator-authored **voice tree** and per-stream **stream trees**, join proofs derived from the
room key, coordinator heartbeats + link pulses, split-brain resolution by (epoch, lower peer id),
elections and failover, spam reports that kick/ban. Media (audio/video units) flow as datagrams
down the trees; the desktop bridge attaches with fixed slots (16 audio + 4 video).

### Failure modes

| Failure | Behaviour | Visible? |
|---|---|---|
| Callee offline when invited | invite waits in the outbox; caller sees a pending call | **no "callee unreachable" signal distinct from "ringing"** until the invite acks or TTL |
| Coordinator (caller) dies | election by (epoch, peer id); a new coordinator takes over; media heals in ~0.5–1 s | self-healing (tested in `-scenario call`) |
| A member's link drops | heartbeats stop → parent-lost reports → reparent/adopt; voice resumes | self-healing |
| Stale / forged / wrong-epoch signal | rejected (`rejectLocked`), warn vs debug by severity | logged |
| Media never connects (NAT) | relayed through the tree via relay-capable members | depends on a usable relay path |

---

## 4. Media streams

A broadcaster calls `StartStream(id, kind, quality)` where quality is a `StreamQualityDTO`
(e.g. `720p30`). The coordinator announces it (`ActionStreamAnnounce`), registers it
(`ActionStreamRegister`), and authors a **stream tree** pushed as `ActionStreamTreeUpdate`;
subscribers `WatchStream` → `ActionStreamSubscribe`, and frames (`PublishVideo`) flow down the
tree. Loss recovery is `ActionNack` (NACK a lost range, served from the upstream buffer) and
`ActionKeyframeRequest`. `StreamClose`/`Unregister` tear it down; a stream missing from a room
sync is closed locally.

### Failure modes

- Subscriber never attached to the tree → **no frames** (watcher stays silent); surfaced in the
  lab as zero `video` units for that stream.
- Lossy link → NACK-driven repair; residual loss shows as fewer units, not corruption
  (`unitOK` guards integrity; the lab counts `damaged`).
- Tree reparent during coordinator failover → a short gap then resume.

---

## 5. Ranked findings (bugs and gaps)

Ranked by delivery impact. "Gap" = works-as-designed but a real limitation; "bug" = behaves
worse than intended.

### F1 — (GAP, high) Offline delivery requires a *shared, previously-used* relay; no DHT mailbox rendezvous
A relay keeps an offline message only for a recipient it has seen connect
(`MailboxManager.RequireRecipientSeen`, [mailbox.go:342](../internal/services/messenger/mailbox.go)).
The sender picks relays from **its own** connected peers ([message_broker.go:318](../internal/services/messenger/message_broker.go));
nothing guarantees any of them is one the recipient has used. If none qualifies, every relay
answers `TypeMailboxFull`, the sender exhausts its relays, and the message sits in the outbox as
`sent`/`offline` until the recipient comes online **directly** (or the 7-day TTL expires). There
is no mailbox-location rendezvous (e.g. a DHT record "peer X's mailbox is relay R"). In the
current bootstrap topology (everyone dials s1/s2, which are relays) this is masked, so it will not
bite until the network has many relays and few shared ones. **Impact:** silent non-delivery to an
offline peer at scale, indistinguishable from an ordinary pending message. **Fix:** out of scope
(design: advertise a peer's chosen mailbox relay(s) in the DHT and address the mailbox there) —
documented, and the lab's offline flow asserts the *masked* case works and traces the relay leg.

### F2 — (BUG, fixable — FIXED) Relay delivery of stored mail is invisible to observability
`RelayStats.Delivered` and `.MailboxWaiting` are maintained
([relay_server.go:74](../internal/p2p/routing/relay_server.go)) but the testnode `/metrics`
endpoint exposed only `relayStored` and `relayForwarded`
([testbed/testnode/vpn.go](../testbed/testnode/vpn.go)). The single most important offline-delivery
event — *a relay handing a stored message to the recipient when it reconnects* — could not be
observed, so a lab could not tell "stored but never delivered" from "delivered". **Fix applied:**
`/metrics` now also returns `relayDelivered` and `relayMailboxWaiting`. The messaging scenario uses
these to trace the offline leg.

### F3 — (BUG, minor) `deliver` reports success before the relay accepts the message into its mailbox
`sendViaRelay` returns success as soon as **one** relay accepts the transport write
([message_broker.go:325](../internal/services/messenger/message_broker.go)); the relay may then
refuse to store it and reply `TypeMailboxFull` asynchronously. The message is marked `StateSent`
in the meantime, over-promising to the UI, and the first retry is wasted re-sending to a relay
that is only marked full *after* that reply arrives. Not lost (it is retried and the relay
rotated), but the state is briefly wrong. **Fix:** out of scope (would need a synchronous relay
accept/-reject) — documented.

### F4 — (GAP) No group message history backfill
See §2. A member that is offline past the outbox TTL, or that joins later, never receives earlier
group messages. **Fix:** out of scope (design: a group history sync on join) — documented.

### F5 — (GAP) A call to an offline callee is indistinguishable from "ringing"
See §3. The invite is a tracked message; there is no "callee unreachable" signal distinct from the
normal pending state. **Fix:** out of scope (UI/protocol: surface invite `StateOffline`/`StateKeys`
as a distinct call state) — documented; the lab traces invite delivery explicitly.

### F6 — (GAP, minor) Receipts for plain/ChaCha messages are always X3DH
`sendAck` always uses `TierX3DH` ([outbox.go:299](../internal/services/messenger/outbox.go)). If
two peers share only a ChaCha/plain relationship and have no ratchet session, the receipt is parked
behind a knock/key exchange, so the sender keeps retransmitting the original until X3DH converges.
Works eventually; adds latency and traffic. **Fix:** out of scope — documented.

### F7 — (GAP, observed in the lab) The relay mailbox is bypassed when the sender holds a half-open link
Observed directly by `-scenario messaging`: when the recipient is killed abruptly, the sender keeps
"successfully" writing to its still-open QUIC link (the idle timeout has not elapsed), so `deliver`
reports success and the message is **never handed to a relay mailbox**. Offline delivery then relies
entirely on the sender's own outbox flushing when the link truly dies and is re-established on
reconnect — not on relay store-and-forward. The lab run showed exactly this: for the offline flow
no relay's `stored` counter moved, yet the message arrived after the recipient returned. This is not
data loss (the outbox is durable for 7 days), but it means the mailbox path — the only mechanism
that survives the **sender** also going offline before the recipient returns — is not exercised in
the common "peer crashed" case. **Fix:** out of scope (would need `deliver` to detect a stale link
faster, e.g. a shorter keep-alive / write confirmation before reporting success) — documented. The
scenario reports which mechanism carried each offline message rather than assuming the mailbox.

---

## 6. What is solid

- 1:1 delivery, dedup (`firstSight`), receipts and the retry/backoff loop are coherent and the
  session-desync self-heal is real and tested (`TestBrokenSessionHeals`,
  `TestMessageToOfflineContactIsDeliveredLater`).
- Group delivery correctly reuses the 1:1 path with per-member accounting.
- The call stack's failover (coordinator death, link cuts, lossy links) is exercised and heals
  (`-scenario call`).
- Mailbox quotas and the `recipient-seen` anti-abuse rule are sound; `TypeMailboxFull`
  blacklisting works.

---

## 7. How this is proved in the lab

`go run ./testbed/runner -scenario messaging` brings up the standard topology (public relays
s1/s2, LAN nodes a1/a2, b1/b2, c1/c2 behind NAT routers gw_a/gw_b/gw_c) and runs six asserted
flows across different NATs: a 1:1 DM, a group message, **offline store-and-forward** (recipient
taken down, message sent, recipient restarted, arrival asserted), a 1:1 call, a group call, and a
media stream. Each flow is recorded as a *delivery trace* (did it leave the sender; which relays'
counters moved; did it reach the recipient; was it acked). The run ends with a **DELIVERY SUMMARY**
printed last: failures first, each with its trace, then every flow's trace. See
[runner/messaging.go](runner/messaging.go).
