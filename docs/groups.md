Русская версия: [groups.ru.md](groups.ru.md)

# Groups and spaces

A group in EchoFog works like a community server — channels, voice rooms, roles, invitations,
moderation — but there is no server. Everything a group is lives in one encrypted log that every
member keeps a copy of, and every change in it (a message, a new channel, a role, a ban) is signed
by whoever made it. Every member checks every change the same way, so nobody needs a server to
tell them what is allowed.

That has two consequences worth knowing up front:

- **Somebody of the group has to be reachable.** Joining, getting the keys and new messages needs a
  connection to at least one member, directly or through a relay (see
  [When nothing arrives](#when-nothing-arrives)). A [group file](#the-group-file-fogzip) shows the
  history without one.
- **What a role may not do is refused by everyone,** not hidden by a server. A member who is not
  allowed to post links can write one, but no other member's app will take the message.

## Spaces: channels, categories, voice rooms

A group (a *space*) has a main channel and any number of text channels and voice rooms. The column
on the left lists them.

- **Categories.** A new space shows two headings, *Text* and *Voice*. The first time you change
  one (rename, remove, add a channel to it), both become categories of their own: you can rename
  them, remove them and put text channels and voice rooms in any of them. *New category* (the
  folder button next to the space's name) adds more. Drag a channel onto a heading to file it
  there; a channel in no category sits on top. Folding a category is remembered on your computer.
- **The main channel** has settings like any other (the gear on it, or a right click): its name,
  topic, *Only admins post*, slow mode, and which channel the space opens on.
- **A channel's settings:** name, topic, *Only admins post* (an announcements channel: others can
  still comment in threads), slow mode, category, and *Permissions here* (below).
- **Slow mode** keeps a member from writing in a channel more often than every N seconds (5 s to
  1 h). Moderators and those who manage channels are not held back. It is kept by the sender's
  app: the log has no clock everybody trusts, so a modified app could ignore it.
- **Threads.** Right-click a message → *Comment*. The thread's author or a moderator can name it.
  The speech-bubble button at the top lists the channel's threads.

## Roles and permissions

Every member has a role. The built-in ones are *owner*, *admin*, *moderator*, *member* and
*restricted* (a channel's readers are *subscribers*). In the space's panel (gear next to the
name), **Roles** lists them with how many members hold each.

Who may manage roles can:

- **change a built-in role** (its permissions, name and colour; its rank stays);
- **make a role of their own** with a name, a colour, a rank from 1 to 99 and any permissions;
- **give a member a role** from the member list.

Two rules hold everywhere, checked by every member's app: you only change roles **below your own
rank**, and you cannot grant a permission **you do not have yourself**. The owner (rank 100) is
never changed by anybody.

The permissions:

| Group | Permission |
| --- | --- |
| Messages | write, send files, post links, comment in threads, react, pin |
| Voice | join calls and voice rooms, share a screen or a camera |
| Moderation | delete others' messages, remove members, ban members |
| Management | invite, manage channels, manage roles, edit the group's profile |

### Permissions in one channel

A channel can let a role do more or less than in the rest of the group: *Permissions here* in the
channel's settings. Pick *Everyone* or a role, and for each of the channel's permissions choose
**✕** (not here), **/** (as in the group) or **✓** (here too). *Everyone* applies first, then the
member's role; the owner is never held back. Examples: a `#news` channel where only moderators
write, a voice room where only *Speakers* may talk, a channel where newcomers may not post links.

Hiding a channel from a role is **not** possible yet: all members hold the group's keys, so a
hidden channel would need keys of its own.

## Inviting people

In the space's panel, **Invitation**:

- **The quick invitation** is an https link valid for a week. Messengers make it tappable; it opens
  a page that starts the app (or offers to download it). *Copy for the app* gives the same
  invitation as `fogroup2:…`, which you can also paste into the app.
- **New invitation** makes one with a note ("for the stream"), how long it lets people in (an hour
  to 30 days), how many people (1 to 100, or no limit), the role they get (if you manage roles),
  and **With approval**: then nobody gets in by the link alone — an admin accepts each newcomer.
- The list shows each invitation, how many joined with it, until when it works and who made it.
  Its maker, or anybody who manages roles, can **revoke** it: nobody new comes in with it, those
  who joined stay.

The count of uses is kept in the group's log, so every admin sees the same and a used-up or
revoked invitation lets nobody in, whoever lets the newcomer in. (Two admins accepting people at
the very same moment may go one or two over the limit.)

In a private group nobody can put themselves on the member list without an invitation; knowing the
group's ID is not enough.

### Requests to join

With an approval invitation, the newcomer's request shows up for everybody who may let members in:
a badge with the number of requests on the space in the chat list, a bar on top of the chat, a
sound, and **Requests to join** in the space's panel with *Accept* and *Decline*. The first admin
who accepts lets the person in; the others' copies of the request go away. Declining tells the
person, and the group goes from their list.

### What the newcomer sees

Right after joining, the space is on their list with a clock mark and a bar explaining the wait:

- *You asked to join: an admin of the group has to accept you* — an approval invitation;
- *Waiting for a member to let you in* — the request reached a member, the member list, keys and
  history are on their way;
- *No member of the group can be reached yet* — see [When nothing arrives](#when-nothing-arrives).

They cannot write until they are let in. The history, threads and channels arrive then (or at once
from a group file).

## The group file (.fogzip)

Instead of, or together with, a link, you can hand somebody the group in a file: in the space's
panel, *Save the group to a file (.fogzip)…*. The file holds the invitation and what the group's
log holds — members, roles, channels, threads and messages (not the files people posted) — sealed
with a password of at least 8 characters. Send the file one way and say the password another.

The person opens it with **Open a link** (Ctrl+L) → *Open a group file (.fogzip)…*, types the
password, and reads the group at once, even while no member is online. Opening it also joins the
group, as the link does, so the request goes out (and waits for approval if the invitation asks
for it).

Every message in the file is signed by its author and checked on import: whoever passes the file
on cannot change or add anything. What the file cannot do is let somebody in: the keys for new
messages, the approval, and calls still need a member.

## Moderation

- **Timeouts.** In the member list, *Timeout* keeps a member of lower rank from writing and reacting
  for 10 minutes, an hour, a day or a week; they stay in the group and read. *Lift timeout* ends it
  early. What they try to send meanwhile is refused by every member, and stays refused after the
  lift. (A modified app that stamps its messages with a time after the end gets them in, shown at
  that time.)
- **Remove** and **Ban** take a member out; a ban keeps them out and every message they write
  afterwards is refused, however they date it.
- **Moderation log** (folded, at the bottom of the panel): who let whom in, removed, banned, gave a
  role or a timeout, set up or removed roles and invitations, and whose messages a moderator
  deleted. It is read from the group's log, so it is the same for everybody and cannot be tidied up
  afterwards.

## How a group keeps in sync

Each member's changes are numbered (1, 2, 3, …) and signed. A new message goes to every member at
once in a small group, and to a few connected members in a big one (over 50), each of whom passes it
on to a few more. On top of that:

- a member who notices a gap in somebody's numbers asks the member who sent the message right away;
- in big groups, who passes a message on also tells twelve more members "I have up to number N of
  X"; who lacks it asks for it;
- every two minutes, and whenever two members connect, they compare a short fingerprint of what
  they hold and fill each other's gaps.

The details are in [Group encryption](group-encryption.md) and [How it works](how-it-works.md).

## When nothing arrives

If a newcomer sees nothing, a creator sees no request, or a call does not connect, the two devices
most likely cannot reach each other:

- **Both behind NATs that cannot be punched through** (a *symmetric* NAT, most mobile networks).
  Then everything goes through a **relay**, and if no relay is connected nothing goes at all. The
  app now says so: *No way to the recipient right now* on a message, *No way to them* on a call, and
  *No member of the group can be reached yet* on a group.
- **What helps:** UPnP or NAT-PMP on your router (a forwarded port makes a symmetric NAT reachable),
  a relay in the network (any node that relays — your own on a VPS, or a friend's), or a group file
  to read the history meanwhile. See [Reachability](reachability.md).

Server operators: a node run as *signaling only* relays nothing. If the only servers in reach are
signaling-only, people behind such NATs cannot write to or call each other. See
[Run a node](run-a-node.md#bandwidth-and-cost).
