Русская версия: [getting-started.ru.md](getting-started.ru.md)

# Getting started with the EchoFog desktop app

EchoFog is a peer-to-peer VPN that is hard to block, with free end-to-end encrypted chat and calls
built in. This guide walks you through the desktop app, step by step. It takes about ten minutes.

> **Status: experimental.** EchoFog has not had an independent security audit by a specialist
> organization yet, and it has not been tested against a real national DPI. Do not rely on it in a
> high-risk situation yet. See the [FAQ](faq.md) for what it does and does not protect.

## 1. Get the app

Download the app from the project's [Releases page](https://github.com/AdmiralCore/echofog/releases).

- **Windows:** the file is called `EchoFog-<version>-windows-amd64.exe`. There is nothing to
  install: just run it. The file is not code-signed yet, so Windows SmartScreen may warn you about
  an unknown publisher.
- **Linux:** there is also a Linux build, `EchoFog-<version>-linux-amd64`. It needs the WebKitGTK
  4.1 library (`libwebkit2gtk-4.1`) on your system.

Each release also has a `SHA256SUMS` file. If you know how, check that your download matches it.

## 2. First start: profile, password, recovery phrase

1. **Create a profile.** Type a name under *New profile name* and press *Create*. A profile is
   just a separate account on this computer; you can have several.
2. **Create an identity.** Choose *New identity*, then pick a password (at least 8 characters) and
   repeat it. The password encrypts your keys on this computer. **It cannot be reset.**
3. **Write down the recovery phrase.** The app shows you a list of words once. Write them down on
   paper, in order, and keep them offline. Then tick *I have written the phrase down* and press
   *Continue*.

Why the phrase matters:

- It is the **only backup**. If you forget the password or lose the computer, the phrase brings
  back your identity and your wallet (*Forgot password* → *Recovery phrase*).
- **Anyone who has the phrase controls your identity and your wallet.** Never type it into a
  website or send it to anyone, including people who say they are from EchoFog.

Already have a phrase from another computer or a server node? Choose *Recovery phrase* instead of
*New identity* and type the 12 or 24 words.

## 3. Turn on the VPN

Open **Proxy** in the left menu. Pick how to choose an exit — an exit is another person's computer
your traffic goes out to the Internet from:

- **Automatic** — the app checks exits in the background and picks the one that opens your sites
  fastest for the best price. *Check sites* lets you choose which sites it tests.
- **Choose myself** — pick an exit from the list. You can sort by price, ping and speed.
- **Specific node** — connect through one computer you know, for example a friend who shares
  their connection with you for free, or a hidden exit you were invited to. Paste the link
  (`echofog://join?…` or `echofog://relay/…`), `host:port/peer-id` or peer ID they gave you.

Press **Connect**. When it says *Connected*, you have two ways to use it:

- **SOCKS5 for apps** (the default). The app shows a local address such as `127.0.0.1:…`. Only
  programs you set to use this SOCKS5 proxy (for example, a browser) go through the exit.
- **Route the whole computer through the exit.** Turn on this switch and every program goes
  through the exit. It **asks for administrator rights**. Read the warning under the switch: there
  is no kill switch yet, so if the app stops unexpectedly, programs fall back to your normal
  connection and your real IP can show for a moment.

## 4. Paying for traffic

Some exits are free. Others set a price per GB, paid in TON or USDT from the app's own wallet
(**Wallet** in the menu). Many exits give a little free test traffic first. When an exit needs
payment, the Proxy screen shows how to pay it.

The cheapest way to pay is a **deposit**: you put TON into your own small contract once, and the
exits take only what you used, every few hours. How this works, and its current testnet status:
[vault-payments.md](vault-payments.md).

The wallet is for traffic only. Its keys live on this computer, so do not keep savings in it.

## 5. Share your connection (optional)

You can also be an exit yourself: other people's traffic then goes out to the Internet through your
computer, and they pay you your price (or nothing).

1. Decide **who** may use it, under **Proxy → Access to this computer**:
   - **Public** — anybody can find your exit in their list and use it at your price.
   - **Hidden** — nobody can find it; only the people you send your **invitation link** to can use
     it. The mode changes after you restart the app.
2. In **Share your connection** set the price in dollars per GB (`0` is free) and press
   **Start sharing**. A public exit also needs your upload speed (*Measure* fills it in).
3. Public: you are done, people find you on their own. Hidden: copy **Your invitation link** from the
   *Access* card and send it to your people; they paste it into **Specific node**.

Friends who should never pay go under **Free for these people**. **The traffic of the people you
serve leaves from your IP address**, and the legal responsibility is yours: read
[What happens if I share my connection?](faq.md#what-happens-if-i-share-my-connection) first. All the
details of the hidden mode, in plain words: [Hidden and isolated nodes](hidden-nodes.md).

## 6. Chats, groups and calls

Everything here is free and end-to-end encrypted.

- **Give your contact.** In **Chats**, press the QR-code button (*My contact*). Show the code or
  press *Copy link* and send the link to a friend.
- **Add a contact.** Press the pencil button at the top of **Chats** (*New*) and choose
  *Add a contact*. Paste their link (or key), type a name and press *Add contact*. Or press *Scan*
  and point the camera at their code.
- **Create a space.** *New* → *New space*, type a name, choose *Group* (everyone writes) or
  *Channel* (only admins write) and press *Create*. To invite people, press the gear next to the
  space's name and copy the invitation: an https link messengers make tappable. You can also make
  invitations with a term, a limit, a role or approval, or save the whole group to a password-sealed
  file (.fogzip) that shows its history at once. They join with *New* → *Join by invite*, or
  **Open a link** (Ctrl+L). Roles, per-channel permissions, categories, timeouts and the moderation
  log: [Groups and spaces](groups.md).
- **Channels and voice rooms.** In a space, the column on the left lists its text channels and
  voice rooms. Admins add them with *+*, and change a channel with the gear that shows when you
  hover over it (or a right click): name, topic, "only admins post", order, remove. You can also
  drag channels to reorder them. Click a voice room to join it; the people inside are listed under
  it.
- **Threads.** Right-click a message and choose *Comment* to start a thread. The speech-bubble
  button at the top of a space lists the channel's threads.
- **A contact's profile.** Click the name at the top of a chat. Click the name to rename them and
  the picture to set one (only you see both). Below are *Media*, *Files* and *Links* from your chat.
- **Call.** In a chat, press the phone button for a voice call or the camera button for a video
  call. In a space with voice rooms you call by joining a room.
- **Share your screen.** During a call, press *Share your screen*. The arrow next to it opens
  *Stream quality*: you can pick up to 4K at 60 fps, but high quality needs a fast upload. To share
  the sound too, pick a whole screen and tick *Share system audio* in the window that opens (or a
  browser tab with *Share tab audio*). Only people watching your stream hear it, and each of them
  can mute it with the speaker button on the stream.
- **Strangers and blocking.** When somebody who is not in your contacts writes to you, the chat
  shows up with a *request* label. They cannot call you or send files until you press *Accept*
  (answering them accepts them too). To stop hearing from somebody, open their chat, press the
  info button at the top and choose *Block*: their messages, calls, files and group invitations no
  longer reach you, and they are not told. Who may write to you and who may call you (by default
  only your contacts) is under **Settings** → *Privacy*, together with the list of blocked people.

### Hide your address: Tor and the route of calls

**Settings** → *Tor and proxy* sends the app's own traffic (messages, the network) through Tor or
another SOCKS5 proxy, so the EchoFog network does not see your IP address: choose *Tor Browser*
(keep it open), *Tor service* or *My SOCKS5*, press *Check*, then *Save* and restart the app. Through
a proxy the app uses TCP only and is slower, and the whole-computer VPN mode is off.

Tor carries no UDP and is too slow for video, so **calls** have a route of their own on the same
card: *Automatic* (direct where possible, through a relay when Tor or guards are on), *Direct*
(fast, but the person you call sees your address) or *Through a relay* (only the relay sees it;
quality depends on the relay).

## 7. Theme and language

Open **Settings** → *Appearance*. Choose the theme (*System*, *Light*, *Dark*) and the language
(*System*, English, Русский). *System* follows your Windows settings.

## 8. If the app shows no connection

If a banner says **No connection to the network**, the built-in entry nodes may be blocked where
you are. The app joins the network through *bootstrap nodes* — the first nodes it connects to.

1. Get a node address from a friend who runs a node, or from the [EchoFog channel](https://t.me/EchoFogDev).
2. Press **Add a node** on the banner (or open **Settings** → *Bootstrap nodes*).
3. Paste the address and press **Add and connect**. It looks like `echofog://relay/…` or
   `host:port/peer-id`. The peer ID part is required.

The chip next to *Bootstrap nodes* shows how many peers you are connected to.

If a message says *No way to the recipient right now*, a call says *No way to them*, or a space you
joined says *No member of the group can be reached yet*, you and the other side are both behind
routers that cannot be punched through and no relay is connected. It goes once one is; turning on
UPnP on your router usually fixes it at once. See [Reachability](reachability.md#when-both-sides-are-hard-to-reach).

A different message, *This computer cannot be reached from the Internet*, is not a problem for
you as long as a relay is in reach: chats, calls and the VPN still work. It only matters if you want to serve others as a relay or
a public exit (a hidden exit for your own people works without it). See
[reachability.md](reachability.md).

## 9. Logs and feedback

If something does not work, open **Logs**. *Copy all* copies the log; *Save* writes it to a file in
your home folder. If the app closes by itself (for example right after the password), the reason is
in `crash.log` next to the log file (`%LOCALAPPDATA%\EchoFog` on Windows). A database damaged by a
power cut or a crash while saving is repaired at the next start, with a copy kept beside it.

**Connection report.** *Logs* → *Connection report* puts together what the app knows about how it
connects — country, provider, NAT, which transports get through, guards, the VPN and the latest
problems — without any IP address, ID, link or contact. Nothing is sent: *Open the report form on
GitHub* opens an issue with it filled in, for you to read and post. Reports that it works help as
much as reports that it does not.

**Check before you share.** Passwords, keys and the recovery phrase are blanked out, and IP
addresses are masked automatically, but node IDs, sites you checked and contact names can stay in
the logs. Read what you send, and post logs only where you trust the readers.

Send feedback and bug reports as an [issue on the project's GitHub page](https://github.com/AdmiralCore/echofog/issues), in the [EchoFog channel](https://t.me/EchoFogDev), or in the [community chat](https://t.me/+3N3EKWXi-ow3MzBi). Security problems: see [SECURITY.md](../SECURITY.md).

## Next

- [FAQ](faq.md) — short, honest answers about privacy, payments and risks.
- [Groups and spaces](groups.md) — channels, roles, invitations, moderation, the group file.
- [How it works](how-it-works.md)
- [Run a node](run-a-node.md) — share your connection or help others get online.
- [Hidden and isolated nodes](hidden-nodes.md) — share your connection only with the people you
  choose.
