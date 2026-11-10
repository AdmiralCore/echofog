Русская версия: [getting-started.ru.md](getting-started.ru.md)

# Getting started with the EchoFog desktop app

This guide is for 0.3.0, the first release on protocol v2. It walks through the first start, contacts,
spaces, voice rooms and calls, files, and the built-in VPN. Short answers to common questions are in
the [FAQ](faq.md).

## Get the app

Download the release for Windows or Linux from the
[releases page](https://github.com/AdmiralCore/echofog/releases). Each release is signed; how to check
that a download is genuine is in [Release trust](release-trust.md).

On Windows, the app is not signed with a code-signing certificate yet, so SmartScreen may warn about an
unknown publisher. Check the release first, then choose *More info* → *Run anyway*.

## First start: profile, password, recovery phrase

1. Choose **Create a profile** and set a password. It unlocks the encrypted database on this computer.
2. The app shows a **recovery phrase**. Write it down on paper and keep it safe. With the phrase you can
   restore your identity on another computer; without it, a lost password means a lost profile. Nobody,
   including us, can recover it for you.
3. The app connects to the network through its built-in entry nodes. You can see and change them in
   *Settings* → *Network* → *Entry nodes*: turn the built-in ones off, add your own, or check whether an
   entry answers.

*Remember on this computer* (Windows) opens the profile without the password. It is convenient, but then
anyone who can use your Windows account, or any program running in it, can open the profile.

To restore a profile on another computer, choose **Restore** (*Restore from recovery phrase*). Your identity, and so your
contacts' view of you, stays the same; the new computer is a new device of the same account.

## Contacts

Your link is under your name in the profile card. Send it to someone through any channel; when they
open it in EchoFog (or paste it with **Ctrl+L**), they send you a request, and you accept it. Links also
come in an https form that messengers make tappable: the page opens the app or offers the download.

A link can have a term, and a person who is not your contact cannot write to you until you accept them.

## Spaces, channels and voice rooms

The narrow bar on the left is the rail. At the top is **Personal** (your direct chats and *Saved
messages*), below it your spaces, and **+** to create a space or join one by link.

- **Create a space**: **+** → *Create*. Add text channels and voice rooms, group them in categories, and
  set roles and per-channel permissions in the channel settings (the gear next to a channel).
- **Invite people**: the space menu → *Invite*. An invitation can have a term and can require your
  approval for each request.
- **Moderate**: slow mode and "only admins write" per channel, removing and banning members, the log of
  what moderators did.
- **Voice rooms**: one click on a room joins it. While you are in a room you can open any text channel;
  the call shrinks to a mini window, and *Back to call* brings it back.
- **Hide the column** of channels with **Ctrl+B** when you need room for the chat or the stream.

Replies, threads, reactions, edits, pins, mentions (`@name`, `@all`), notifications per channel and
muting for a term work in every chat.

## Calls and screen streams

- **Call** a contact from the chat header, or start a call in a group.
- **Share your screen** from the call bar: pick a screen or a window and the quality, up to 4K at 60 fps.
  The stream reaches viewers over a tree of their own uplinks, so it does not need a server; viewers who
  cannot keep up get a lower layer automatically.
- The route of calls is in *Settings* → *Tor and proxy*: *Direct* is fastest but shows your address to
  the person you call; *Through a relay* hides it from them.

## Files and voice messages

Drag a file into a chat or use the clip. A file sent into a group lands in the same channel and thread
for everyone. Large files resume after a break.

Files you receive are saved as ordinary, **unencrypted** files in your download folder. Keep that in mind
for sensitive files; an encrypted media store comes after 0.3.0.

When someone is offline, a message waits in an encrypted mailbox for up to 7 days. Its state is shown
honestly: *waiting for connection*, *in mailbox*, *delivered* or *expired*.

## The VPN

Open **VPN** at the bottom of the rail.

1. Paste the link to an exit that someone gave you and press *Connect*.
2. Choose how to use it:
   - **Apps through a proxy**: point a browser or another app to the SOCKS5 proxy the screen shows
     (`127.0.0.1:1080` by default);
   - **The whole computer**: everything goes through the exit, with a kill switch. If the tunnel drops,
     the kill switch keeps apps offline instead of letting them out directly.
3. If the exit refuses, the screen says why (busy, no path, a policy of the exit, and so on) and what
   helps.

If the kill switch is holding the network closed and you need the normal connection back, use *Disable
protection* on the VPN screen. Details: [Windows](kill-switch-windows.md), [Linux](kill-switch-linux.md).

## Share your own exit

You can let chosen people go online through your computer. **Their traffic leaves from your IP address**,
and you answer for it, so read [the FAQ](faq.md#what-happens-if-i-share-my-connection-as-an-exit) first.

On the VPN screen, open *Share your connection*:

- by default only the contacts you pick can connect, and each new request waits for your confirmation;
- *Anyone with the link* is a separate switch with a term, and you can revoke it at any time;
- the screen shows who is connected, and you can disconnect anyone;
- sharing turns off after every restart.

## Tor and proxy

*Settings* → *Tor and proxy* sends the app's own traffic through Tor or another SOCKS5 proxy, so the
EchoFog network does not see your address. In this mode the app does not punch through routers and does
not announce your address; calls go through relays. It is slower, and it only helps if Tor or the proxy
is reachable from your network.

## When something does not work

- **No connection**: check *Settings* → *Network* → *Entry nodes* (*Check* shows which entries answer), or
  open an invitation link from someone already in the network.
- **A message hangs in "waiting for connection"**: neither side can reach the other right now and no
  mailbox has it yet; it is delivered when one of you is online with a path.
- **Connection report**: *Settings* → *Diagnostics* → *Connection report* gathers what the app knows about
  its connection, without addresses or IDs, ready to post in an issue.
- **A crash** is written to `crash.log` next to the log (`%LOCALAPPDATA%\EchoFog` on Windows).

## Next

- [Groups and spaces](groups.md): roles, permissions, invitations and moderation in detail.
- [FAQ](faq.md): safety, logs, legal risks.
- [SECURITY.md](../SECURITY.md): what is and is not protected.
- Run your own node: [VPS with protocol v2](deploy-vps-v2.md).
