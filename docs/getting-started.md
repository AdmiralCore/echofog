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

1. **Create a profile.** Type a name into the field that says *New profile name* and press
   *Create*. A profile is just a separate account on this computer; you can have several.
2. **Create an identity.** Choose *New identity*, then pick a password (at least 8 characters) and
   repeat it. The password encrypts your keys on this computer. **It cannot be reset.**
3. **Write down the recovery phrase.** The app shows you a list of words once. Write them down on
   paper, in order, and keep them offline. Then tick *I have written the phrase down* and press
   *Continue*.
4. **Choose who can find this computer.** *Public* (recommended) is listed in the network like any
   other computer: if you share your connection, anyone can find it and use it at your price.
   *Hidden* is not listed anywhere and answers only the people you give your invitation link to,
   your contacts and those who joined before; you still use the network as usual. Press
   *Continue*. You can change it later in **Proxy → Access to this computer**.

Why the phrase matters:

- It is the **only backup**. If you forget the password or lose the computer, the phrase brings
  back your identity and your wallet (*Forgot password* → *Recovery phrase*).
- **Anyone who has the phrase controls your identity and your wallet.** Never type it into a
  website or send it to anyone, including people who say they are from EchoFog.

Already have a phrase from another computer or a server node? Choose *Recovery phrase* instead of
*New identity* and type the 12 or 24 words. If the phrase was made with an extra passphrase, open
*The phrase was made with an extra passphrase* and type it there. Then pick a new password.

To delete a profile, press the bin next to it in the profile list. It asks for that profile's
password, so a web page cannot wipe your data on its own.

## 3. Turn on the VPN

Open **Proxy** in the left menu. Pick how to choose an exit — an exit is another person's computer
your traffic goes out to the Internet from:

- **Automatic** — the app checks exits in the background and picks the one that opens your sites
  fastest for the best price. *Check sites* lets you choose which sites it tests.
- **Choose myself** — pick an exit from the list. Sort it by *Best*, *Ping*, *Price* or *Speed*.
  *Test* next to an exit checks whether it answers, how many test sites open through it and how
  fast it is.
- **Specific node** — connect through one computer you know, for example a friend who shares
  their connection with you for free, or a hidden exit you were invited to. Paste the link
  (`echofog://join?…` or `echofog://relay/…`), `host:port/peer-id` or peer ID they gave you.

*Switch to another exit if this one stops working* is on by default: if the exit disappears, stops
sharing or no site opens through it, the app moves to another working exit. Your programs keep the
same proxy address.

Press **Connect**. When it says *Connected*, you have two ways to use it:

- **SOCKS5 for apps** (the default). The app shows a local address such as `127.0.0.1:…`. Only
  programs you set to use this SOCKS5 proxy (for example, a browser) go through the exit.
- **Route the whole computer through the exit.** Turn on this switch and every program goes
  through the exit. It **asks for administrator rights**.

Next to it is the **Kill switch**, on by default. If the exit drops, you switch exits, the network
changes or EchoFog crashes, programs stay offline instead of going out on your real address, until
an exit serves you again. Read the note under the switches: it does not cover everything yet. If
the helper process itself is killed, or Windows removes the EchoFog adapter, the normal connection
comes back, and DNS is kept inside the tunnel only on a best-effort basis. With the kill switch
off, programs fall back to your normal connection when something goes wrong, and your real IP can
show.

If EchoFog stopped while the whole computer went through the exit, the kill switch keeps the
internet blocked. The login screen and the Proxy screen say so. Connect to an exit and it comes
back by itself, or press *Unblock the internet*.

**Your exits** lists the exits you used and what each one gave, kept encrypted on this computer
only. Star one to make it a favourite, rename it, copy its link, connect to it again or forget it.
While you are not connected, *Connect to the last exit* takes you back in one click.

## 4. Paying for traffic

Some exits are free. Others set a price per GB, paid in TON or USDT from the app's own wallet
(**Wallet** in the menu). Many exits give a little free test traffic first.

When you pick a paid exit, a box says *This exit is paid* with its price and how many KB a day
are free. Payments come from your deposit if you have one, otherwise straight from your wallet.
Tick *Pay automatically, up to $… a day* to let the app top up by itself. While you are connected,
the *Paying this exit* card shows what is left and what you spent today, where it is paid from
(*Deposit* or *Wallet*), the daily limit and *Pay $… now*.

The cheapest way to pay is a **deposit**: you put TON into your own small contract once, and the
exits take only what you used, every few hours. You make it in **Wallet**, on the *Deposit for paid
nodes* card: *Deposit* puts TON in, *Withdraw* takes the rest back (it takes about a day). How this
works, and its current testnet status: [vault-payments.md](vault-payments.md).

The wallet is for traffic only. Its keys live on this computer, so do not keep savings in it.

Sends you start yourself above 1 TON (or 1 USDT) ask for a yes in a window of the system. Smaller
ones add up: once the sends of the last hour reach that amount, the next one asks too. A payment
to an exit that a page asks for goes through the same check. Paying for the traffic you use runs
on its own and asks nothing.

## 5. Share your connection (optional)

You can also be an exit yourself: other people's traffic then goes out to the Internet through your
computer, and they pay you your price (or nothing).

1. Decide **who** may use it, under **Proxy → Access to this computer**:
   - **Public** — anybody can find your exit in their list and use it at your price.
   - **Hidden** — nobody can find it. It answers only the people you send your **invitation
     link** to, your contacts and those who joined before. The mode changes after you restart the
     app.
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
- **What you can do with a message.** Right-click it (or press the arrow on it) for its menu:
  quick reactions, *Reply* (or double-click the message), *Comment*, *Edit*, *Pin*, *Copy text*,
  *Save message*, *Save as text file*, *Details* and *Delete for everyone*. Some of them show only
  on your own messages or for your role. A pinned message shows in a bar at the top of the chat;
  click the bar to go to it. Ctrl+F searches the chat: Enter goes to the earlier match, Shift+Enter
  to the later one.
- **Threads.** *Comment* in a message's menu starts a thread. The speech-bubble button at the top
  of a space lists the channel's threads.
- **Saved messages.** *Save message* keeps a message in *Saved messages*, at the top of the chat
  list.
- **Files.** Send a file with the paper clip, or drop it on the chat. The other side presses
  *Download* or *Decline*; a picture shows a small preview at once. A file you posted in a group
  stays on offer until you press *Stop sharing*. How it works: [Sending files](file-transfer.md).
- **A contact's profile.** Click the name at the top of a chat. Click the name to rename them and
  the picture to set one (only you see both). Below are *Media*, *Files* and *Links* from your chat.
- **Call.** In a chat, press the phone button for a voice call or the camera button for a video
  call. In a space with voice rooms you call by joining a room. *Deafen* turns off all sound for
  you. Hover over a person's tile and press *More* to set their *User volume* or *Mute for me*.
- **Watch a stream.** A stream someone shares starts for you only when you press *Watch stream*
  on it. Hover over its tile for *Stream stats*, *Switch relay* (when the stream lags or shows
  garbage), *Stop watching* and *Full screen*.
- **Share your screen.** During a call, press *Share your screen*. The arrow next to it opens
  *Stream quality*: you can pick up to 4K at 60 fps, or 1080p and 1440p at 120 fps, but high
  quality needs a fast upload. *What to keep* sets what gives way when the computer or the network
  falls short: *Smoothness* (games, films: the picture gets softer, the frame rate holds) or
  *Sharpness* (text, code, slides: frames are skipped, text stays sharp). While you share, the menu
  shows what really goes out and whether the graphics card or the processor encodes it. To share
  the sound too, pick a whole screen and tick *Share system audio* in the window that opens (or a
  browser tab with *Share tab audio*). Only people watching your stream hear it, and each of them
  can mute it with the speaker button on the stream.
- **Strangers and blocking.** When somebody who is not in your contacts writes to you, the chat
  shows up with a *request* label. They cannot call you or send files until you press *Accept*
  (answering them accepts them too). To stop hearing from somebody, open their chat, press the
  info button at the top and choose *Block*: their messages, calls, files and group invitations no
  longer reach you, and they are not told. Who may write to you and who may call you (by default
  only your contacts) is under **Settings** → *Privacy*, together with the list of blocked people.

## 7. Hide your address

### Tor and the route of calls

**Settings** → *Tor and proxy* sends the app's own traffic (messages, the network) through Tor or
another SOCKS5 proxy, so the EchoFog network does not see your IP address: choose *Tor Browser*
(keep it open), *Tor service* or *My SOCKS5*, press *Check*, then *Save* and restart the app. Through
a proxy the app uses TCP only and is slower, and the whole-computer VPN mode is off.

Tor carries no UDP and is too slow for video, so **calls** have a route of their own on the same
card: *Automatic* (direct where possible, through a relay when Tor or guards are on), *Direct*
(fast, but the person you call sees your address) or *Through a relay* (only the relay sees it;
quality depends on the relay).

### Anonymous mode

**Settings** → *Anonymous mode* makes sure nothing in the app shows your address:

- *Through Tor*: nobody in the EchoFog network sees it (set Tor above first). Slower.
- *Only my relay*: the app connects to the relay you pinned under *Joining the network* and to
  nothing else, so only that relay sees your address. Fast enough for calls and streams; the VPN
  then works only with that relay as its exit.

Either way calls and files go through relays, nothing punches holes, maps ports or asks STUN, and
the update check and the location lookup go through Tor or the VPN or are skipped. The TON wallet
stays offline unless you allow it (its servers would see your address). If the mode cannot work
(no Tor set, no relay pinned), the app connects nowhere rather than in the open. While it is on,
the sidebar shows *IP hidden* (*Through Tor*) or *IP: own relay only* (*Only my relay*). It applies
when the app next starts.

What it does not cover: the hosting of your relay knows who pays for it, and how and when you write
can give you away. For a public group, use a separate profile from your personal one.

## 8. Settings: look, sound, network, links

- **Theme.** *Appearance* chooses the theme: *System*, *Light* or *Dark*.
- **Language.** The *Language* card chooses *System*, English or Русский. *System* follows your
  Windows settings.
- **Sounds.** The *Sounds* switch turns the app's sounds on or off; set their volume and listen to
  each one.
- **Network.** *Direct socket* skips NAT discovery for top speed, only for a computer with a
  public IP. *UDP socket buffer, MB* sets the socket buffer. Both apply after a restart.
- **Links.** *Open an EchoFog link* → *Open a link* (or Ctrl+L) takes a contact link, a group
  invitation or a node's link (`echofog://join?…`, `echofog://relay/…`) and offers what fits: add
  the contact, join, use it for the VPN, add it as a relay. A link pasted anywhere outside a text
  field opens it too, and so does an `echofog://` link clicked in a browser.
- **Updates.** When a new version is out, a banner says *EchoFog v… is available*, with
  *Download from GitHub*.

## 9. If the app shows no connection

If a banner says **No connection to the network**, the built-in entry nodes may be blocked where
you are. The app joins the network through *bootstrap nodes* — the first nodes it connects to.

1. Get a node address from a friend who runs a node, or from the [EchoFog channel](https://t.me/EchoFogDev).
2. Press **Add a node** on the banner (or open **Settings** → *Bootstrap nodes*).
3. Paste the address and press **Add and connect**. It looks like `echofog://relay/…` or
   `host:port/peer-id`. The peer ID part is required.

The chip next to *Bootstrap nodes* shows how many peers you are connected to.

**Settings** → *Joining the network* shows how the app goes in. By default (*Automatic*) it goes
through a few camouflaged guard servers where the internet is censored, so your address is not
spread around, and directly elsewhere. *Always through guards* hides your address everywhere, a
little slower; *Direct* is the old way. *Route through your own relay* pins a relay you run or
trust, and the app always goes in through it. This is only about the network: the VPN exit is
chosen on the Proxy page. How guards work: [Anti-DPI](anti-dpi.md#guards-a-users-node-keeps-few-links-across-the-border).

If a message says *No way to the recipient right now*, a call says *No way to them right now*, or a
space you joined says *No member of the group can be reached yet*, you and the other side are both
behind routers that cannot be punched through and no relay is connected. It goes once one is;
turning on UPnP on your router usually fixes it at once. See [Reachability](reachability.md#when-both-sides-are-hard-to-reach).

A different message, *This computer cannot be reached from the Internet*, is not a problem for
you as long as a relay is in reach: chats, calls and the VPN still work. It only matters if you want to serve others as a relay or
a public exit (a hidden exit for your own people works without it). See
[reachability.md](reachability.md).

## 10. Logs and feedback

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
