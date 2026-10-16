Русская версия: [getting-started.ru.md](getting-started.ru.md)

# Getting started with the EchoFog desktop app

EchoFog is a peer-to-peer VPN that is hard to block, with free end-to-end encrypted chat and calls
built in. This guide walks you through the desktop app, step by step. It takes about ten minutes.

> **Status: experimental.** EchoFog has not had an independent security audit by a specialist
> organization yet, and it has not been tested against a real national DPI. Do not rely on it in a
> high-risk situation yet. See the [FAQ](faq.md) for what it does and does not protect.

## 1. Get the app

Download the app from the project's Releases page. <!-- TODO(owner): link -->

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
  their connection with you for free. Paste the link or peer ID they gave you.

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

## 5. Chats, groups and calls

Everything here is free and end-to-end encrypted.

- **Give your contact.** In **Chats**, press the QR-code button (*My contact*). Show the code or
  press *Copy link* and send the link to a friend.
- **Add a contact.** Press *Contact*, paste their link (or key), type a name and press
  *Add contact*. Or press *Scan* and point the camera at their code.
- **Create a group.** Press *Group*, type a name, choose *Group* (everyone writes) or *Channel*
  (only admins write) and press *Create*. To invite people, open the chat's *Info* and use
  *Copy invite*. They join with *Join* → paste the invite.
- **Call.** In a chat, press the phone button for a voice call or the camera button for a video
  call. This works in groups too.
- **Share your screen.** During a call, press *Share your screen*. The arrow next to it opens
  *Stream quality*: you can pick up to 4K at 60 fps, but high quality needs a fast upload.

## 6. Theme and language

Open **Settings** → *Appearance*. Choose the theme (*System*, *Light*, *Dark*) and the language
(*System*, English, Русский). *System* follows your Windows settings.

## 7. If the app shows no connection

If a banner says **No connection to the network**, the built-in entry nodes may be blocked where
you are. The app joins the network through *bootstrap nodes* — the first nodes it connects to.

1. Get a node address from a friend who runs a node, or from the EchoFog channel.
   <!-- TODO(owner): link -->
2. Press **Add a node** on the banner (or open **Settings** → *Bootstrap nodes*).
3. Paste the address and press **Add and connect**. It looks like `echofog://relay/…` or
   `host:port/peer-id`. The peer ID part is required.

The chip next to *Bootstrap nodes* shows how many peers you are connected to.

A different message, *This computer cannot be reached from the Internet*, is not a problem for
you: chats, calls and the VPN still work. It only matters if you want to serve others. See
[reachability.md](reachability.md).

## 8. Logs and feedback

If something does not work, open **Logs**. *Copy all* copies the log; *Save* writes it to a file in
your home folder.

**Check before you share.** Passwords, keys and the recovery phrase are blanked out, and IP
addresses are masked automatically, but node IDs, sites you checked and contact names can stay in
the logs. Read what you send, and post logs only where you trust the readers.

Send feedback and bug reports as an issue on the project's GitHub page, or in the EchoFog channel.
<!-- TODO(owner): link --> Security problems: see [SECURITY.md](../SECURITY.md).

## Next

- [FAQ](faq.md) — short, honest answers about privacy, payments and risks.
- [How it works](how-it-works.md)
- [Run a node](run-a-node.md) — share your connection or help others get online.
