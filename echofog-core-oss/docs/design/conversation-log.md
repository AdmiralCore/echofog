# Conversation log — design

Status: accepted, in implementation. Date: 2026-10-27. Branch: `exp/reality-quic-sni`.

Replaces "independent encrypted messages with dedup" as the model of a chat with a
**per-conversation, append-only log of signed, typed events**, synced between participants.
Every future chat feature (replies, threads/comments, edits, deletes, reactions, pins, polls…)
becomes a new event kind instead of a change to transport or storage.

Prior art this follows: per-author append-only logs merged into one deterministic order
(Holepunch Autobase, which powers Keet rooms; p2panda; Secure Scuttlebutt), version-vector
anti-entropy (the classic replica-sync technique), LWW registers per field and add/remove sets for
reactions (local-first CRDT practice), recipient-chosen mailboxes found through a published record
(the idea behind Session's swarms, without a deterministic swarm mapping). Range-based set
reconciliation (Negentropy, Nostr NIP-77) was considered and rejected for now: it targets
unstructured sets; per-author sequence numbers give us the same "send only the difference" with a
much simpler protocol. It stays the fallback if the log ever stops being per-author.

---

## 1. What the code does today (constraints the design must respect)

From `testbed/MESSAGING-AUDIT.md` and a full map of the messenger:

- DM history (`m:` keys) is ordered by the **receiver's local arrival second** plus a local counter.
  No message-id index, no update/delete, no causality on the wire. `reply_to_id` exists in
  `PersonalMessageProto` but nothing sets it.
- Message ids are random UUIDs; group ids too. Timestamps mix ns (DM), ms (groups), s (DHT).
- Groups already have per-sender, per-epoch sealed envelope chains (`senderkeys`, `Envelope.N`)
  with envelope sync (`requestSync`/`answerSync`) and an admin history handover. The envelope
  cache is bounded (7 days, 3000 per group). The group content signature
  (`echofog-gsk-content-v1`) is fixed and `Extra` is reserved for files.
- All protocols are multiplexed by **text prefixes** (`fogack1:`, `fogrp2:`, `fogcall://`, …)
  through one interceptor slot; acks and `seen/` dedup run before it.
- Storage: `Set/Get/Delete/FindValues/FindValuesSeek` only — no batch, no transaction, no reverse
  iteration. Personal values must be under `SealedPrefixes`; identifiers in keys must be blinded.
- DHT accepts records only under `bundle:<peer>` and the VPN advert key. The bundle record is
  JSON (`peer_id, pub_key, bundle_raw, sni`) and already carries a hint (`sni`).
- Relays store offline mail only for a recipient they have *seen* (in memory, 24 h), and the
  sender picks relays from its own peers — so offline delivery needs a shared relay (audit F1).

## 2. The model

### 2.1 Conversations

| Kind | Conversation id (16 bytes) |
|---|---|
| Direct | `SHA-256("echofog-conv-dm-v1" ‖ min(pubA,pubB) ‖ max(pubA,pubB))[:16]` |
| Group | `SHA-256("echofog-conv-group-v1" ‖ groupID)[:16]` |

Both sides compute the same id with no negotiation.

### 2.2 Events

`internal/services/convlog/convlog.proto`:

```proto
message Event {
  bytes  conv     = 1;  // 16-byte conversation id
  bytes  author   = 2;  // author's 32-byte Ed25519 identity public key
  uint64 seq      = 3;  // per (conv, author), starts at 1, +1 per event, never reused
  uint64 lamport  = 4;  // Lamport clock: max(lamport of everything the author had seen in conv)+1
  int64  ts_ms    = 5;  // author's wall clock; display only, never used for order
  uint32 kind     = 6;  // see 2.5; unknown kinds are stored and synced, not rendered
  bytes  target   = 7;  // event this one acts on (edit, delete, react, pin)
  bytes  reply_to = 8;  // message being answered (quote/reply)
  bytes  thread   = 9;  // root message of the thread this message belongs to (comments)
  bytes  body     = 10; // kind-specific payload (protobuf)
  bytes  sig      = 15; // Ed25519 by author over the deterministic encoding of fields 1..10
}
```

- **Event id** = `SHA-256("echofog-ev-v1" ‖ signed bytes)[:16]`, 32 hex characters. Content
  addressed: any participant can verify that a reference points to exactly this event; ids cannot
  collide or be forged.
- The signature covers everything, so a peer that relays or serves an event during sync cannot
  change it. Authors are identified by public key, so verification needs no lookup.
- A duplicate `(conv, author, seq)` with a different id is an **equivocation** (a forked author,
  e.g. a restored backup). Both events are kept (they have different ids) and rendered; the
  author's seq chain is flagged, not trusted for gap detection past the fork.

### 2.3 Order

Display order is the total order `(lamport, event id)`, identical on every device (the id breaks
ties between concurrent messages; it is a hash, so the index names no one). A reply, edit or
reaction always has a larger Lamport value than its target, so causality is respected. An
event whose Lamport value jumps more than 2^24 above the local maximum is rejected (clock-inflation
spam). `ts_ms` is shown as the time but never sorts.

### 2.4 Transport vs. log

The **log holds plaintext signed events** (sealed at rest by the storage layer).
Confidentiality comes from the transport:

| Path | Encryption |
|---|---|
| DM live delivery | the existing X3DH/Double-Ratchet session (event bytes inside `PersonalMessageProto`) |
| Group live delivery | the existing sender-key envelope of the current epoch |
| Sync (both) | the pairwise session between the two syncing peers |
| Relay mailbox copies | whatever the live path used — relays only ever see ciphertext |

This decouples history from key retention: a member who has events can hand them to another
member long after the epoch keys that carried them were pruned.

### 2.5 Kinds and merge rules (CRDT)

All rules are commutative and idempotent: the result does not depend on arrival order, and the
same event applied twice changes nothing (dedup by id first).

| Kind | Body | Rule | Who may |
|---|---|---|---|
| `MSG` (1) | text, type (text/photo/voice/file/location/system), attachment, metadata | append | participants |
| `EDIT` (2) | new text | LWW register on the target, ordered by `(lamport, author, seq)` | target's author |
| `DELETE` (3) | — | tombstone; wins over every edit; permanent | target's author; group admins with delete permission |
| `REACT` (4) | emoji, on/off | LWW per `(target, emoji, reactor)`; reactor = author | participants |
| `PIN` (5) | on/off | LWW per target, conversation-wide | DM: both; group: members with pin permission (admins by default) |
| unknown | opaque | stored, synced, ignored by the view | — |

Replies set `reply_to`; thread messages (comments) set `thread` to the root message. A thread is
just the messages whose `thread` is the root; the root keeps a reply count and last-activity time.

An op can arrive before its target (out of order, or the target is still being synced). The view
keeps a **stub** state for the target id holding the merged ops; it becomes visible when the
target's `MSG` arrives. No op is ever lost to ordering.

Forward compatibility is the point of the unknown-kind rule: an old client still stores and
forwards a kind it does not understand, so mixed-version groups do not lose new features' events.

## 3. Storage

New sealed prefix `cl/`. `bc` = blinded conversation id, `ba` = blinded author (both via the
storage blinder). Values are sealed.

| Key | Value | Purpose |
|---|---|---|
| `cl/e/<bc>/<ba>/<seq be64>` | event bytes | source of truth; range reads for sync |
| `cl/i/<bc>/<id>` | `{author, seq}` | id → event; "already applied" marker |
| `cl/o/<bc>/<^lamport be64>/<id>` | id | history, **newest first** by forward scan (`^` = MaxUint64−x) |
| `cl/s/<bc>/<id>` | message view (JSON) | text after edits, deleted, reactions, pin, reply/thread refs, reply count |
| `cl/t/<bc>/<root>/<lamport be64>/<id>` | id | thread view, oldest first |
| `cl/p/<bc>/<id>` | pin record | list of pins |
| `cl/v/<bc>` | conversation meta (JSON) | kind, peer/group, version vector, own seq, max Lamport, last activity, unread |

Writes of one event go in **one atomic batch** (new optional `storage.Batcher`, a single badger
transaction; sealed wrapper seals each value; stores without it fall back to sequential writes in
the order e → s/o/t/p → i → v so a crash re-applies safely: `i` is written after the view, so a
re-delivered event that was half-applied is applied again; the view updates are written to be
idempotent).

Existing `m:` and `groups/m/` history is imported once into the log as **local legacy events**
(kind `MSG`, flagged legacy, not synced, ids derived from the old ids). Old keys are kept, read-only,
for rollback.

## 4. Live delivery

- `PersonalMessageProto` gets two fields: `bytes conv_event = 9` and `bytes conv_sync = 10`.
- The broker gets a dedicated sink (`SetEventSink`) instead of another text prefix: after
  decryption and origin authentication, a message with `conv_event`/`conv_sync` goes to the
  conversation log and is not stored under `m:`.
- For `MSG` the sender also fills `text` (and `id` = event id). An old receiver therefore still
  shows the message; a new one uses the event. No flag day.
- Ops (`EDIT`, `DELETE`, `REACT`, `PIN`) are sent only to peers known to speak the log
  (capability `CapConvLog`, learned from the handshake, cached per contact, and published in the
  bundle record as a feature flag).
- Tracking: `MSG`, `EDIT`, `DELETE` go through the outbox (delivery state, retries, relay mailbox).
  `REACT`, `PIN` are sent once without tracking; sync heals any loss.
- The outbox/ack id of a `MSG` is the event id, so delivery state (`keys/offline/sent/delivered`)
  maps 1:1 onto a log entry.

## 5. Sync (anti-entropy)

Per-author append-only logs make the difference between two replicas a set of seq ranges, so a
**version vector** (author → highest contiguous seq) is enough.

```
SyncHello  { items: [ {conv, digest} ] }              // digest = 8 bytes of SHA-256(sorted VV)
SyncWant   { conv, have: [ {author, seq} ] }           // sent when digests differ
SyncEvents { conv, events: [bytes], more: bool }       // events the asker lacks, by author then seq
```

- Both sides send `Hello` on connect (DM: the one conversation with that peer; groups: every group
  both are in). Equal digests end the exchange at one message per conversation.
- Different digests: each side sends `Want` with its full vector; the other answers with every event
  whose seq is above the asker's entry, in pages of at most 200 KB, `more=true` until done.
- Gaps: the vector stores the highest *contiguous* seq, so a hole left by out-of-order live delivery
  is asked for again; events already held are dropped by id.
- Groups additionally run periodic anti-entropy with up to 3 random members (the existing
  `syncLoop` cadence: 20 s after start, then every 10 min).
- Authorization: DM — only the two participants. Group — the asker must be a current member; a
  removed or banned peer gets nothing. New members receive history by default (as the admin
  handover does today); a per-group setting can limit it to events after `JoinedAt`.
- Equivocating authors: the vector entry stops at the fork point and both branches are exchanged.

## 6. Mailbox rendezvous (audit F1)

Offline delivery must not depend on sharing a relay by luck.

1. Each node picks up to 3 **mailbox relays** among the relays it is connected to: the ones in use
   while they stay connected, then the longest-connected (`pkg/node/mailbox_rendezvous.go`, every
   2 min). Never a peer it chats with (a contact or a message request): its availability is tied
   to the very conversations the mailbox stands in for.
2. **Registration is the connection.** A relay notes every connected peer as a recipient it keeps
   mail for; the note is now persisted (key `mbs:<blinded peer>`, peer id in the sealed value, 24 h
   TTL) and refreshed for long-lived connections, so a relay restart no longer refuses everyone.
3. It **advertises** them in its bundle record and identity card (`StoredBundleRecord.Mbox`: relay
   peer ids, addresses, expiry; signed with the identity key in `MboxSig`, so a peer passing the
   record on cannot redirect the mail). The bundle is republished at once when the choice changes,
   else every 10 min. No new DHT key type — old nodes store the record unchanged.
4. A sender's relay order puts the recipient's mailbox relays first. When the recipient cannot be
   reached, the connector looks its hints up (cached, else the DHT record), connects to the relays,
   and flushes the recipient's queue through them.
5. The recipient stays connected to its mailbox relays and drains them on connect.

Privacy cost: the bundle record reveals which relays a node uses for mail. Acceptable (contacts
already know the node id); documented in the user docs.

## 7. Groups

- **Per-group mode.** The group wire version is now 3 ("speaks the log"; 2 = sealed messages). A
  group runs on the log once every member is known to speak version 3 (`groupsFacade.LogMode`);
  until then it runs the old way, its messages are mirrored into the log as legacy messages, and
  the operations (replies, threads, edits, deletes, reactions, pins) return `ErrNeedsUpgrade`.
- **Transport.** A group event is sealed with the group's sender key in the same `smsg` envelope as
  a sealed message, its plaintext marked by `fogcev1\0` (`pkg/api/node/group_log.go`). It therefore
  gets the epochs, the envelope cache, the per-sender gap repair and the envelope sync that sealed
  messages have; `takeEnvelope` hands it to the log and drops it unless its author is the member
  that sealed it. A node of version 2 cannot parse it as a message and ignores it.
- **Sync** of group logs uses section 5 between members as well (on connect for the groups both are
  in, and every 10 min with up to 3 connected members), so history no longer depends on the
  envelope cache (7 days, 3000 per group).
- **Large groups** (above `meshLimit` = 50): the author pushes to at most 8 members, connected ones
  first; everyone else converges through envelope sync and log sync. O(1) per message for the
  author instead of O(members).
- **Epoch keys.** Rekeys stay paced (2 min, up to 30 min under churn), so after a removal the
  epoch in use may still be held by the removed member for a while (*tainted*). Nothing new is
  sealed with a tainted epoch any more: in either mode the message or event goes to the remaining
  members in their own 1:1 sessions until the new epoch is out. (This was a real gap: messages sent
  in that window were readable by the member just removed.)
- **Bans** are persisted in the group record and restored on start; before, a ban lived only in
  memory and the banned peer could be added back after a restart.
- **Membership on the log** (`convlog/members.go`, kind `MEMBER` = 6). The founder (named by the
  signed genesis) opens a group's membership with a **roster** (who is in it, with which role);
  after it every join, role change, removal and ban is an event of the group's log, written by
  whoever did it. What an author may do at a point of the log follows from the membership events
  *before that point* in log order, so every device decides the same whatever the arrival order,
  and a member who was removed keeps what they wrote while in the group (a newcomer who syncs
  later sees it). Rules: a join is self-written (anyone holding the invitation, with the default
  role) or written by a member with `PermManageRoles`; role changes and removals need the
  permission and a higher rank (the owner is untouchable; ownership moves only from the owner, who
  becomes an admin); bans need `PermBanMembers`. Before the roster the roster's own state counts;
  until a group's log has a roster at all, the group's present state decides, as before. The
  founder writes the roster at creation, and for a group from before on its next start; an admin
  writes the join of a newcomer it lets in, and a member that finds itself left out after a sync
  asks to be let in itself (once an hour at most).
- **Permissions at that point.** Participant = member then; writing = `PermSendMessages` (so a
  channel's readers read, react, and do not write); pin = `PermManageGroup` or
  `PermDeleteMessages`; deleting others' messages = `PermDeleteMessages`.
- **Refused events are kept.** An event that is well formed and signed by somebody in the
  conversation (or in its membership history), but not allowed, is stored without effects: it
  fills its author's sequence, so the version vector moves on and sync stops offering it, and a
  rebuild weighs it again. Events from total strangers are dropped.
- **Rebuild.** A membership event that sits before events already applied (or held) marks the
  conversation; its views are computed again from the stored events in log order (live: at once;
  sync: once at the end of the page). Legacy history keeps its messages and places. Sync answers
  send membership events first, so most events apply right the first time.
- **Sync guards.** A sync page that moves nothing (the digest did not change) is not asked for
  again; a peer gets at most 40 sync messages per 10 s.
- **Big groups** pass a fresh event on to 3 connected members each (once, the first time they
  have it), so it reaches everybody in a few hops instead of waiting for anti-entropy.
- **Log mode** waits only for members last heard speaking an older version; one never heard from
  counts as current (second-hand lists only ever tell that somebody is newer).
- The legacy `KeyRotationEngine` is left in place: it never protected messages and still does not
  (sender-key epochs do); removing it is cleanup for later.

## 7a. Spaces: channels and voice rooms

A group is a **space**: one log, with named parts (`convlog/channels.go`).

- A message names its channel in the signed `Event.ext` (`EventExt.channel`); the main channel
  is the empty id, which every message from before channels is in. The ext is kept when a deleted
  message's body is stripped, so a channel never loses its messages' places.
- Kind `CHANNEL` = 7 (`ChannelBody{id, name, voice, removed}`) creates, renames and removes a
  channel; last writer wins per id; the channels are ordered by their earliest write. Writing it
  needs `PermManageGroup` or `PermManageRoles` at that point of the log (rights as in section 7).
- Storage: `cl/c/<bc>/<id>` the channel, `cl/q/<bc>/<id>/<^lamport>/<id>` its history (the main
  channel keeps `cl/o`); the meta keeps a last message, unread count and read point per channel.
- Membership, rights, sync and the rebuild are the group's: a channel needs no keys, roster or
  sync of its own, and a space of many channels costs one log.
- A **voice room** is a channel with `voice` set. Joining it joins the group's call in that room:
  group calls are keyed by room (wire field `rm`), and a room's call is announced to the members as
  going on (`callactive`) and never rings. The group's own call (room "") rings as before.

## 8. Delivery fixes

| Finding | Fix |
|---|---|
| F7 half-open link | sender side: a message the peer took directly but did not confirm after 2 retries also goes to its mailbox on each further retry (the peer drops the second copy). Relay side (the lab's case: the relays, not the sender, held the dead link): a relay forwarding to a recipient it has not heard from for 15 s pings it after the forward and keeps the message in the mailbox when there is no answer within 5 s; one ping covers everything forwarded meanwhile. The only hot-path cost is an atomic timestamp per inbound frame |
| F5 call to an offline callee | caller: the invite's message state maps to "not reachable" (keys/offline/relayed), "calling" (sent), "ringing" (delivered); callee: an invitation older than 2 min raises `call:missed`, not a ring |
| F3 premature "sent" | new state `relayed`: a relay took it while the recipient is away; `sent` now means the recipient's device took it |
| F6 receipts | **won't fix, by design**: only the X3DH/ratchet tiers authenticate the sender, and a receipt must be authenticated (else anyone holding a pre-shared key could forge "delivered") |
| relay drop on reconnect | a relay hands stored mail to a reconnected recipient on connect and again ~10 s later, and drops it only then (it used to drop it as soon as the first write went out, losing it if that frame on a fresh connection was lost) |

## 9. API

Spaces: `ChatRef.channel` picks a channel for history, sending and reading; `ChatChannels`,
`ChatChannelSave(ref, id, name, voice)` (id "" creates) and `ChatChannelRemove`; voice rooms are
joined with `StartRoomCall(group, room)`, kept with `KeepRoomCallAlive` and ended with
`EndRoomCall`.

The chat list comes whole from `ChatRows` (`Conversations`, Wails `ChatList`): contacts, requests
and groups, each with `kind` (direct/group/channel), `name`, presence, block, role, member count
and `can` (send, files, react, pin, manage, invite, call). Every message carries `kind`
(text/file/call/system), a normalized `file`, and `can` (edit, delete, pin, react, reply); group
members carry `canKick`, `canBan`, `roles` and `canTransfer`. A `conv:change` with `reload` means
the chat was rebuilt; `rev` orders changes. The app holds no copy of the rules.


Core (`pkg/api/node/conversations.go`), all identifiers hex strings, timestamps in
**milliseconds**; a chat is a `ChatRef{kind: "dm"|"group", id}`:

- `Conversations() []ConversationDTO` — with `unread`, `request` (a stranger's chat) and
  `upgraded` (the chat runs on the log)
- `ChatHistory(ref, cursor, limit) ChatPage` — newest first, cursor based; thread comments excluded
- `ChatThread(ref, rootID, cursor, limit) ChatPage` — oldest first
- `ChatPins(ref)`, `ChatMessage(ref, id)`
- `ChatSend(ref, SendRequest{text, replyTo, thread}) ChatMessage`
- `ChatEdit`, `ChatDelete`, `ChatReact(ref, id, emoji, on)`, `ChatPin(ref, id, on)`, `ChatMarkRead`
- `SubscribeConversations(cb)` → desktop event `conv:change` (`{ref, conversation, messages}`);
  delivery states keep coming as `message:state` (now with `relayed`); a call invitation older than
  2 min raises `call:missed` instead of `call:incoming`.

The desktop exposes them as Wails methods in `echofog-desktop-oss/app_chat.go`. The old methods
(`SendMessage`, `GetMessages`, `SendGroupMessage`, `GetGroupMessages`) stay and are served from the
log.

## 10. Testing

- Unit: event encode/sign/verify/id, every merge rule in every arrival order (permutation tests),
  stubs, equivocation, Lamport cap, version vector and digest, sync paging and gap refill,
  authorization, batch atomicity and crash re-apply.
- Integration (multi-node, `-p 1`): DM send/edit/react/pin/delete across nodes, offline peer catches
  up by sync, mixed old/new nodes, group events with a removed member, large-group convergence.
- Lab: `testbed/runner -scenario messaging` extended with the new kinds and the mailbox-rendezvous
  path; its delivery trace reports which mechanism carried each event. Testbed runs never in
  parallel.
- `-race` in WSL.

## 11. Phases

1. `convlog` core (events, store, view, sync) + DM integration + API.
2. Mailbox rendezvous.
3. Groups on the log; large-group push + anti-entropy; epoch rotation on removal; persisted bans.
4. F7, F5, F3, F6.
