Русская версия: [group-encryption.ru.md](group-encryption.ru.md)

# Group encryption

How group messages are protected, how they reach members who were offline, and what this does
not protect against. For the overview of the whole system see [How it works](how-it-works.md).

## In short

- Every group message is **signed by its author** and **sealed once for the whole group** with
  a key only current members hold.
- The sealed message travels to each member **inside that member's own end-to-end encrypted
  session**, as before. Nothing is weaker than it was: the new layer sits on top.
- Because the sealed message is the same bytes for everybody, **any member can hand it on** to a
  member the author could not reach. The signature tells the reader who wrote it, whoever passed it
  along.
- When somebody **leaves or is removed**, an admin makes a **new key** for those who stay. What is
  said afterwards cannot be read with the keys the departed member had.
- **History** an admin passes to a newcomer carries the authors' signatures, so it cannot be
  changed on the way.

## Keys: epochs and sender chains

An **epoch** is a 32-byte random secret for one group. An admin makes it, signs it with their
identity key together with the group, the epoch number, its tag, the creator, the time it was made
and the list of members it is made for, and sends it to every member over their session. Members accept an epoch only from someone who is
an admin of the group in their own view, and only with a valid signature.

Each member who writes derives their own **chain** from the epoch:

```
chain_0     = HKDF-SHA256(epoch secret, salt = group, info = "echofog-gsk-chain-v1|" + sender)
message key = HMAC-SHA256(chain_n, 0x01)
chain_n+1   = HMAC-SHA256(chain_n, 0x02)
```

Every message uses the next key of the author's chain once; used keys are wiped. Messages that
arrive out of order are fine: the keys of skipped messages are kept (up to 2000 per chain) until
those messages arrive. An envelope that does not open never moves a chain forward, so junk cannot
push a chain past real messages.

## The sealed message (envelope)

```
group | epoch number | epoch tag | sender | message number | nonce (24 bytes) | ciphertext
```

The ciphertext is XChaCha20-Poly1305 under the message key; everything before it is the additional
data, so none of it can be changed. The nonce is random even though each key seals one message: a
crash that loses the chain's position before it is saved would otherwise reuse a key with the same
nonce.

Inside is the **content**: group, message ID, time, author, text and (for a posted file) the file
offer, with the author's **Ed25519 signature** over all of it (the file offer as its SHA-256). Any
member can seal under the epoch, so the seal alone does not prove authorship; the signature does.

In a group on the conversation log (every member on version 3), the log's events travel in the
same envelopes: messages, edits, deletions, reactions, pins, roles, member changes, the group's
name and picture. While the epoch in use is tainted (see below), events are not sealed: they go to
each member inside that member's own session.

## Who makes a new epoch, and when

| Event | Who makes the next epoch |
|---|---|
| The group is created | the creator (epoch 1) |
| An older group without an epoch | its owner, the next time they write or a member joins |
| An admin removes or bans somebody | that admin, right away |
| Somebody leaves | the admin who made the epoch in use, or the owner |
| The epoch is older than 7 days | the admin who made it, or the owner |

An epoch is replaced as soon as one of the people who may hold it is gone: those it was made for,
and those this member learned of as members after it got the epoch (they may have got it handed
over). Both times of that check are the member's own clock, so a creator whose clock runs ahead
cannot hide a holder. Two admins removing two people
at the same moment each make an epoch that the other's victim still received; once both removals
are known, the epoch is replaced again by one made for the members who are left. Every member
picks the same epoch without talking: the higher number, and for the same number the smaller tag.

### Against key churn

Somebody leaving and joining in a loop, or being kicked and let back in, must not make the admins
mint key after key:

- In a quiet group the epoch is replaced at once. After an epoch, the next one waits **2 minutes**;
  every departure in between is covered by that one replacement.
- When the replacement comes due, a departed member who is **back in the group** is not a reason any
  more (they would get the new epoch on joining anyway). If everybody is back, no epoch is made.
- With more than three epochs in the last hour the wait doubles with each further epoch, up to
  **30 minutes**.
- An admin answers the same newcomer (member list, history, telling the others) at most once a
  minute; the keys are handed over every time.

In that wait the epoch in use is **tainted**: somebody who is no longer a member may hold it.
Nothing new is sealed with it. New messages and log events go to each member inside that member's
own session, the way older versions send them, until the new epoch arrives. The departed member is
sent none of them.

A newcomer does not get a new epoch made for them. The admin who lets them in hands over every
epoch it holds: the current one and the replaced ones it still keeps (up to 64, for 7 days), and
any member answers a newcomer's request for keys the same way. So a newcomer can open the sealed
messages of the last week that members keep. In a group on the conversation log the newcomer also
syncs the whole log, and can read the group's history from its start. Older groups hand a newcomer
the last 100 messages. Do not count on earlier messages being hidden from people you let in.

Members keep an epoch for 7 days after it was replaced (at most 64), because a message written in
it can still arrive that late from somebody who was offline.

## Delivery and catching up

In a group of up to 50 members the author sends the sealed message to every member. In a bigger
group on the conversation log it sends it to 8 members (connected ones first), each of them passes
it on to 3 more, and the rest get it when they compare logs (see below). A member the author sent
it to who is offline gets it from the author's outbox when both are online again, as before. If the author is offline by then, the other
members fill the gap:

- every member keeps the sealed messages of the last week (up to 3000 per group);
- every 10 minutes, and when it notices a gap in an author's message numbers, a member asks a few
  others (members it heard from recently first) what it is missing; it sends where it stands in
  each author's chain;
- the asked member answers with the sealed messages the asker lacks, in batches of up to 48 KB,
  and with any epoch the asker does not hold.

A message that arrives before its epoch waits and is opened as soon as the epoch comes; the member
asks for the epoch at once.

The group's conversation log (messages, threads, channels, roles; see [Groups](groups.md)) catches
up on its own as well: members compare their logs every two minutes and whenever they connect, a
member asks the sender at once when a message shows a gap below it, and in groups of more than 50
members whoever passes a message on also tells twelve more members how far it holds each author, so
that a lost message is asked for within seconds.

## Changes that arrive out of order

Changes to a group (owner, roles, removals) travel separately from each author, so a member who was
offline can get them in the wrong order: the new owner handing the group on before the old owner
made them owner. Such a change is kept and tried again after each change that is applied, for up to
a week, so every member ends up with the same owner and roles.

## Older versions

Every group message carries the sender's version. A member seals for the members it knows to be
current (from their own messages, or from the member lists admins send) and sends the old way,
unsealed inside the session, to the others. Older members keep taking part; they just do not get
messages handed on by other members, only from their authors.

History from an older member has no signature. A newcomer still takes it on the admin's word, and
the message is marked as not verified.

## What it does not protect against

- **A member's device.** Members hold the epoch secret, and messages are stored on each device
  (encrypted with the user's database key). Whoever controls a member's device reads what that
  member reads.
- **Forward secrecy inside an epoch is limited.** The epoch secret derives every chain of the epoch
  from its start, so someone who records sealed messages and later steals a current epoch secret
  can read that epoch's messages. Since sealed messages travel inside the members' Double Ratchet
  sessions, that also needs those sessions broken. Epochs are replaced on every departure and at
  least weekly.
- **Admins are trusted with membership.** An admin decides who is in the group, and the member
  lists and versions they send are taken as they are. A removed member cannot read new messages,
  but an admin who adds somebody gives them the current epoch.
- **Metadata.** Members see who writes and when. In a group of up to 50 members the apps connect to
  each other, so members can also see each other's IP addresses, unless a link goes through a
  relay (guard mode across a border, anonymous mode). Relays see which peer IDs talk to which,
  when and how much, as with any chat.

## Testing

- `internal/services/groups/senderkeys`: the keys, chains, envelopes and signatures; 98 % of
  statements are covered (the rest are errors that cannot happen with 32-byte keys).
- `pkg/api/node/group_keys*_test.go`: sealing between members, kicks and leaves replacing the
  epoch, two admins removing people at once, a member handing on what the author could not
  deliver, forged messages and forged history, older members, a message arriving before its epoch,
  an older group getting its first epoch, the weekly replacement, restarts, out-of-order owner
  changes; and key churn: ten leave-and-join loops, kicking somebody who keeps coming back, several
  departures sharing one epoch, the growing wait.
- `pkg/api/node/group_fifty_test.go`: **a group of fifty** over 300 random events. People write,
  go offline and come back (often the author is gone again before the reader returns), members are
  kicked, banned and leave, the group changes owner twice and each new owner removes somebody, two
  members run the older version, and six authors drop off for good right after writing. At the end
  it checks that every member who stayed has every message written while they were in the group,
  exactly once and verified; that everybody agrees on the members and the owner; that the members
  hold one epoch made for them only; and that no removed member can open anything written after
  they went. Run another day in the group with `GROUP50_SEED=<n> go test ./pkg/api/node -run
  TestGroupOfFifty`; it has passed on 40 seeds in a row.
