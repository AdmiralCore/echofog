Русская версия: [faq.ru.md](faq.ru.md)

# EchoFog FAQ

Short, honest answers. If something here is unclear or seems wrong, please tell us.

## Is it a VPN or a messenger?

Both, but the VPN comes first. EchoFog is a peer-to-peer VPN: you go online through other people's
computers (exit nodes), not through a company's servers. Its transport is disguised as ordinary
HTTPS to a real site, to make it hard to block. On top of that you get free end-to-end encrypted
chat, groups and calls with screen sharing up to 4K at 60 fps. See [how-it-works.md](how-it-works.md).

## Is it free?

- **Chat, groups, calls and screen sharing:** free, no subscription.
- **VPN:** depends on the exit. Anyone who shares their connection sets their own price per GB,
  and it can be zero. Paid exits are paid in TON or USDT, and many give a little free test traffic
  first. Blockchain transfers also cost a small network fee. See
  [vault-payments.md](vault-payments.md).

## Who sees my traffic?

- **The exit operator** sees which sites and addresses you connect to. For HTTPS sites (the lock
  in the browser) the content stays encrypted. For non-HTTPS traffic the exit can read and change
  the content. The exit also knows your node ID and, usually, your IP address.
- **Your Internet provider** sees encrypted traffic going to some address. The transport is
  built to look like ordinary traffic (for example, HTTPS to a real site). The provider does not
  see the sites you visit through the exit.
- **Messages and calls** are end-to-end encrypted. A relay that carries them sees only
  ciphertext.

Pick exits you have reason to trust, and use HTTPS.

## Is it anonymous?

No. EchoFog makes it hard to see *what* you do and *that* you use a special protocol. It does not
make you anonymous.

- The sites you visit see the IP of the exit, not yours. But the exit sees yours.
- When you talk to a contact directly, peer to peer, your computers can see each other's IP
  addresses.
- Public nodes (relays and exits) are listed in the network's shared directory (the DHT), so a
  censor who crawls it can find them.
- Payments go through the TON blockchain, which is public.

## Is it safe to use in a censored country right now?

Honestly, we do not know yet. EchoFog is experimental. It has not had an independent security
audit by a specialist organization, and it has not been tested against a real national DPI. The
whole-computer mode has no kill switch yet: if the app stops unexpectedly, your real IP can show for
a moment. If being caught is dangerous for you, do not rely on it yet. See
[anti-dpi.md](anti-dpi.md) and [SECURITY.md](../SECURITY.md).

What we can say: in our lab, with a program that reads traffic the way GFW and TSPU are known to,
every VPN tunnel to an exit looked like an ordinary HTTP/3 or HTTPS visit, and
with UDP blocked the VPN came up in under a second. The traffic between nodes still looks like
random bytes, which is easier to flag. A real censor may check things our lab does not.

## Does the disguise slow the VPN down?

Barely. On one computer, one stream carried 40 MB/s over plain QUIC and 37 MB/s with the disguise
(the older random-looking obfuscation: 31 MB/s). The padding against inner-handshake detection
covers only the first few kilobytes of each connection.

What you will notice more is the exit's own limit: a free exit gives each client up to 50 Mbit/s
by default (about 6 MB/s), so that one person cannot take the whole connection. The operator can
change it, and paying clients are not limited. Calls do not go through the exit, so this limit
does not touch them.

## What happens if I share my connection?

Other people's traffic goes out to the Internet **from your IP address**. To websites, and to the
police, it looks like it came from you. Legal responsibility for that traffic is yours, so only
share a connection you are allowed to share.

Some protection is built in. Every exit, public or hidden, refuses to connect to:

- your own computer, your local network and private or reserved address ranges;
- ports used for sending mail (25, 465, 587), so your IP does not end up on spam blocklists;
- Windows file-sharing and RPC ports (135, 137-139, 445) and SSDP (1900), which are common
  targets for scanning and worms.

Everything else goes through. A public exit is published to the network so others can find it; a
hidden one is not, and serves only the people you give a link to (see
[Hidden and isolated nodes](hidden-nodes.md)). Your own log can contain addresses that failed to connect. More in [run-a-node.md](run-a-node.md).

## Can someone overload or attack my node?

They can try, and the node defends itself. Making a new identity is free, so it counts addresses
as well: at most so many peers and identities from one address or subnet, bans for peers that
misbehave, a proof of work for strangers on a server, budgets so that a small request never makes
the node send a lot, and limits on what anyone can take from the relay and the exit. On a VPS the
server role also turns off chat, calls and file transfer, which a relay does not need. What no node
can stop on its own is a flood bigger than its connection: that needs the provider's DDoS
protection. Details in [Protecting a node](node-security.md).

## Why TON? Is there a token?

There is **no EchoFog token**. Payments use TON and USDT on the TON network, because transfers
there are fast and cheap, and a small contract (your deposit) lets you pay per megabyte without a
network fee for every payment. You need no account: your wallet is created from your recovery
phrase.

## Is there a commission?

No. The project takes nothing from payments: the exit gets its full price.

There is an optional, voluntary tip. When a build has a project address set, the Wallet screen
shows *Support the project*: a share (0-20%, 5% unless you change it) added **on top of** what you
pay exits. You can switch it *Off* with one click, and the service does not change.

## Why does it say my computer cannot be reached from the Internet?

Your router or provider does not let other computers connect to you directly — usually because of
NAT, where your router shares one public address among your devices. **Chats, calls and the VPN
still work.** It only matters if you want to serve others as a relay or a public exit. A hidden
exit for your own people works without it, through relays. See
[reachability.md](reachability.md).

## Can I run my own private node for friends?

Yes.

- **Private relay.** Run `echofog-node` on a server with `--relay-private` and list your friends'
  peer IDs in `--relay-allow`. A private relay is not advertised and serves only those people.
  Their nodes reach it by its address (`--relays host:port/<peer id>`). See
  [run-a-node.md](run-a-node.md).
- **Free VPN for friends.** In the app, open *Proxy* → *Share your connection* and add your
  friends under *Free for these people*. They never pay, whatever your price. They connect with
  *Specific node* and the link you give them. On its own this keeps the exit public: everyone else
  can find it and gets your normal price.
- **A VPN only for your people.** Switch *Proxy* → *Access to this computer* to **Hidden** (or run
  a server node with `--access isolated`). Your exit then disappears from the network and answers
  only the people you send its invitation link to. Step by step, in plain words:
  [Hidden and isolated nodes](hidden-nodes.md).

## What does the app log and keep?

- **Your profile** lives in the `.echofog/profiles` folder in your home folder. Messages, contacts,
  groups and other personal data are stored encrypted with a key unlocked by your password.
- **Logs** go to `echofog.log` in the app's cache folder (on Windows,
  `%LOCALAPPDATA%\EchoFog`). The file is capped at 8 MB, plus one previous file. Passwords, keys
  and the recovery phrase are blanked out and IP addresses are masked, but node IDs, sites you
  checked and contact names can stay. Check a log before you share it.
- **Emergency lock** (in *Settings*) erases the decrypted keys from memory at once and stops the
  node. Everything on disk stays encrypted.
- There is no analytics or crash reporting, and the app does not upload logs. Logs leave your
  computer only if you copy or save them and send them yourself.
