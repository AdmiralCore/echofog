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

1. Each node picks up to 3 **mailbox relays** among relay-capable peers it keeps connected (stable,
   reachable; reuse the same ones while they work).
2. It **registers** with them (`MailboxRegister`, signed, renewed every 30 min). The relay persists
   the registration with a 24 h TTL — this replaces the in-memory "recipient seen" gate, so a relay
   restart no longer refuses everyone.
3. It **advertises** them: in the bundle record (`StoredBundleRecord.Mbox`: relay peer ids +
   addresses + expiry; republished with the bundle every 10 min) and directly to contacts when
   connected. No new DHT key type — old nodes store the record unchanged and ignore the field.
4. A sender that cannot reach the recipient directly tries, in order: the recipient's advertised
   mailbox relays (connecting to them if needed), then its own relays as today.
5. The recipient keeps connections to its mailbox relays and drains them on connect.

Privacy cost: the bundle record reveals which relays a node uses for mail. Acceptable (contacts
already know the node id); documented in the user docs.

## 7. Groups

- Group events are `Event`s sealed in sender-key envelopes (new wire kind `cev`), fanned out as
  today to capable members; legacy members keep receiving the old forms.
- Sync of group logs uses section 5 between members, not the envelope cache, so history no longer
  expires with epoch keys.
- **Large groups** (above `meshLimit` = 50): the sender pushes to at most 8 members (online first)
  plus their mailboxes; everyone else converges through anti-entropy. Cost per message becomes
  O(1) for the sender instead of O(members).
- **Epoch keys**: every removal or ban rotates the sender-key epoch at once (no rekey gap), so a
  removed member cannot read anything sent afterwards. The legacy `KeyRotationEngine` (which never
  protected messages) is retired. Bans are persisted.

## 8. Delivery fixes

| Finding | Fix |
|---|---|
| F7 half-open link | a direct send to a peer silent for longer than its keep-alive also leaves a copy in the recipient's mailbox relay |
| F5 call to an offline callee | the invite's message state is surfaced as a call state: `unreachable` (keys/offline) vs `ringing` (delivered) |
| F3 premature "sent" | a relay-path message is `relayed` until a relay accepts it into the mailbox; `sent` after |
| F6 receipts | the receipt uses the tier of the message it confirms |

## 9. API

Core (`pkg/api/node`), all identifiers hex strings, timestamps in **milliseconds**:

- `Conversations() []ConversationDTO`
- `History(chat ChatRef, before Cursor, limit) Page[MessageViewDTO]` — newest first, cursor based
- `Thread(chat, rootID, after Cursor, limit) Page[MessageViewDTO]`
- `Pins(chat) []MessageViewDTO`, `MessageView(chat, id)`
- `Send(chat, SendRequest{text, type, replyTo, thread, attachment}) (MessageViewDTO, error)`
- `Edit(chat, id, text)`, `Delete(chat, id)`, `React(chat, id, emoji, on)`, `Pin(chat, id, on)`
- `MarkRead(chat, upToID)`
- events: `conv:message` (new or changed view), `conv:state` (delivery state by id), `conv:list`

The old methods (`SendMessage`, `GetMessages`, group message calls) stay and are served from the
log. The full frontend contract is in `docs/frontend-conversation-log-spec.ru.md` (repo root).

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
