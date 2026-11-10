Русская версия: [faq.ru.md](faq.ru.md)

# EchoFog FAQ

Short, honest answers for 0.3.0 (protocol v2). If something here is unclear or seems wrong, please tell
us.

## Is it a messenger or a VPN?

A messenger first. EchoFog is a peer-to-peer messenger for communities: groups with channels and voice
rooms, calls, screen streams up to 4K60, files and offline delivery, with no server in the middle. Where
the network is filtered, its connections are disguised as visits to an ordinary website. A VPN is built
in as a second feature: you can go online through an exit someone gives you a link to, or share your
own. See [How it works](how-it-works.md).

## Is it free?

Yes. Chats, groups, voice rooms, calls, streams and files of any size are free, with no subscription
and no account. In 0.3.0 the VPN is free too: exits are shared by link, and there are no payments in
the app. There is no EchoFog token.

## Who can read my messages?

Only the people you write to. Messages, files and calls are end-to-end encrypted: a chat starts with
PQXDH (X25519 together with the post-quantum ML-KEM-768) and continues with a double ratchet. Each
member of a group gets your message over their own session. Relays and mailboxes that carry messages
see only ciphertext.

One limit of 0.3.0: in a group call or a stream, the members who forward the stream to others (the call
travels as a tree of the participants' own uplinks) can see it. They are members of the call anyway; no
outsider can. Details in [SECURITY.md](../SECURITY.md).

## Who sees my traffic when I use the VPN?

- **The exit** sees which sites and addresses you connect to. HTTPS content stays encrypted; anything
  without HTTPS the exit can read and change. The exit also sees your IP address, unless you reach it
  through Tor or a proxy.
- **Your Internet provider** sees encrypted traffic to the exit or to a relay. Across a filtered border
  it looks like a browser visiting the node's own website.
- **The sites you visit** see the exit's address, not yours.

Use exits run by people you have reason to trust, and use HTTPS.

## Is it anonymous?

Not by default. EchoFog makes it hard to see *what* you do and *that* you use a special protocol. Out of
the box it does not hide your IP address from the nodes you connect to.

- When you talk to a contact directly, your computers see each other's address. For calls you can pick
  the route in *Settings* → *Tor and proxy*: *Direct* shows your address to the person you call,
  *Through a relay* shows it only to the relay.
- *Tor and proxy* can send all of the app's own traffic through Tor or another SOCKS5 proxy. Then the
  EchoFog network does not see your address, and the app does not punch through routers or announce
  your address.
- Hidden nodes are not listed in the network's shared directory (the DHT) and answer only invitation
  links. Public nodes are listed, so a censor who crawls the DHT can find them.
- How and when you write can still give you away.

A stricter mode, where everything goes only through your own pinned relays, comes after 0.3.0.

## Is it safe to use in a censored country right now?

We do not know yet, and we will not pretend otherwise. EchoFog 0.3.0 is experimental. It has not had an
independent security audit, and it has not been tested inside a real national censorship system. If
being caught is dangerous for you, do not rely on it yet.

What our lab shows: the disguised QUIC link was not flagged by any of the lab's detectors, and probing a
node without its secret token reaches only the node's website. What it does not show yet: that the TCP
disguise is indistinguishable from a browser (record sizes and timings still differ), anything about
China or Iran, and behaviour in real Russian mobile networks. See [Anti-DPI](anti-dpi.md) and
[SECURITY.md](../SECURITY.md).

## What happens if I share my connection as an exit?

Other people's traffic goes out to the Internet **from your IP address**. To websites, and to the
police, it looks like it came from you. Legal responsibility for that traffic is yours, so share only a
connection you are allowed to share.

The app keeps this narrow by default:

- only the people you choose can connect; access for "anyone with the link" is a separate switch with a
  term, and you can revoke it;
- a new request waits for your confirmation, and sharing is off again after every restart;
- the exit refuses connections to your own computer and local network, to private and reserved
  addresses, to mail-sending ports (25, 465, 587) and to Windows file-sharing ports (135, 137–139, 445)
  and SSDP (1900).

See [Getting started](getting-started.md#share-your-own-exit).

## Can someone overload or attack my node?

They can try, and the node defends itself: caps per address and per subnet, a proof of work for
strangers, limits on what anyone can take from a relay, an exit or a mailbox, bounded tables, and bans.
What no node can stop on its own is a flood bigger than its connection: that needs the provider's DDoS
protection. See [Protecting a node](node-security.md).

## Why does it say my computer cannot be reached from the Internet?

Your router or provider does not let other computers connect to you directly, usually because of NAT.
**Chats, calls and the VPN still work**: the app punches through home routers, including the hard
symmetric kind, and uses a relay when that fails. It matters only if you want to serve others as a
public relay or exit. See [Reachability](reachability.md).

## How do I get into the network the first time?

The released app has a small, signed list of entry nodes built in, so it connects on its own. You can
also add your own entries, or turn the built-in ones off, in *Settings* → *Network* → *Entry nodes*. An
invitation link from a contact or a group works as an entry too. A build from source has no built-in
list: it connects through the links you open.

## What is a .fogzip file?

A group saved to a file by one of its members: the invitation and the group's history, sealed with a
password. Opening it shows the group at once, even with nobody online, and joins it as the link does.
Every message in it is signed by its author, so the file cannot be altered on the way.

## What does the app keep on my computer?

- **The database** (messages, contacts, groups, keys, settings) is encrypted with a key unlocked by your
  password or recovery phrase.
- **Files you receive or save** are ordinary, unencrypted files in your download folder. The system
  viewer, previews, indexing, backups and anyone who can read the folder can see them. An encrypted media
  store is planned after 0.3.0.
- **Logs and `crash.log`** are plain text with technical events. They do not contain message text,
  file names, contact or group names, addresses, keys or tokens. Check a log before you share it anyway.
- **Emergency lock** erases the decrypted keys from memory, stops the node and the VPN and closes the
  app. It is a lock, not a wipe: what is already on disk stays, and the profile opens again with your
  password.
- **Screenshots and screen recording** are not blocked by the app.
- There is no analytics and no crash reporting. Nothing leaves your computer unless you send it.

## Where is the VPN market, payment in TON, the wallet?

Not in 0.3.0. Version 0.3.0 is the first release on protocol v2, and payments, the exit market and
operator clusters come in later updates. Until then exits are free and shared by link.

## The app closes right after I type my password

If the database was damaged by a power cut or a crash, the app repairs it at the next start and keeps a
copy beside it; what the damage reached may be missing. If it still closes, the reason is in
`crash.log` next to the log (`%LOCALAPPDATA%\EchoFog` on Windows): please send it to us.
