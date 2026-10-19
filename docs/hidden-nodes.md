Русская версия: [hidden-nodes.ru.md](hidden-nodes.ru.md)

# Private access: hidden and isolated nodes

This guide is for anyone who wants to share their connection **only with certain people**: family,
friends, a team, or customers who pay you directly. You do not need to be technical: each mode is
explained in plain words first, then there are step-by-step instructions. The technical details are
at the end, for those who want them.

> **Status: experimental**, like the rest of EchoFog. See [Run a node](run-a-node.md).

## In one minute

Normally, when you share your connection in EchoFog, your computer is like a shop on a busy street:
it is on the map, everybody can find it and come in (and pay your price).

Sometimes you want the opposite: a door with no sign on it, that opens only for people who have
the key. That is what the **hidden** and **isolated** modes do.

- You switch your computer (or server) to one of these modes.
- It gives you an **invitation link**. The link is the key.
- You send the link to the people you choose. Only they can connect. Everybody else, including
  scanners that probe the Internet for VPN servers, gets no answer at all: to them it looks like
  nothing is there.

## Words used in this guide

| Word | What it means |
|---|---|
| **Node** | A computer running EchoFog: your desktop app, or the `echofog-node` program on a server. |
| **Exit** | A node that shares its Internet connection. Your traffic leaves to the Internet from the exit, so websites see the exit's address, not yours. |
| **Invitation link** | A line of text starting with `echofog://join?`. It tells the app where your node is and carries the key that lets people in. |
| **Secret** | The key inside the link. Anybody who has the link has the secret, so treat the link like a house key. |
| **Contact** | Somebody in your chat list in the app. |
| **Relay** | Another node in the network that helps two computers find each other when they cannot connect directly. |
| **Behind a router (NAT)** | Most home and mobile connections: the computer has no address of its own on the Internet, so others cannot connect to it directly without help. |
| **VPS** | A rented server in a datacenter, with its own public address. |

## The three modes

### Public (the default)

**What others see:** your node is listed in the network. When you share your connection, your exit
appears in everybody's exit list, with your price.

**Who can use your exit:** anybody, at your price. People on your *Free for these people* list ride
free.

**Choose it when:** you want to earn from strangers, or help as many people as possible.

### Hidden

**What others see:** nothing. Your node is not in any list, it does not help strangers (it is
nobody's relay), and it never tells other nodes its address. If somebody knows its address and tries
to connect without the link, your node does not answer.

**What still works for you:** everything. Your node still uses the network as usual: chats, calls,
your own VPN, finding other people. It just stays invisible while doing so.

**Who can use your exit:** only people with your link, plus your contacts (so friends in your chat
list can still call and write to you), plus people who joined before with an older link.

**Works behind a home router:** yes. The link also names one or two relays your node is connected
to; when a direct connection is impossible, the other person's app reaches you through them.

**Choose it when:** you share from a home computer or laptop, or from a server that cannot open a
port, and you want only your people to use it.

### Isolated

**What others see:** nothing at all. An isolated node does not even join the network: it connects
to no other EchoFog node, not even to the network's entry points. It just sits and waits. (It still
asks public STUN servers and your router what its outside address is, to put it in the link.)

**What works:** only the people with the link, connecting **directly** to it. There are no relays
in its link, so it must be reachable from the Internet: a VPS with a public address, with the
port open.

**Choose it when:** you have a VPS and want a private exit for your people and nothing else. This
is the most closed mode, and the usual choice for a server.

### Side by side

| | Public | Hidden | Isolated |
|---|---|---|---|
| Shown in the exit list | yes | no | no |
| Other nodes learn its address | yes | no | no |
| Helps strangers as a relay | yes (if relay is on) | no | no |
| Uses the network itself (chats, calls, its own VPN) | yes | yes | no |
| Who can use the exit | everybody | link holders, contacts, people who joined | link holders and people who joined |
| Works behind a home router | to use: yes; to serve others: needs an open port | yes, through relays | no, needs a public address |
| Where | app or server | app or server | server only |

### Which one should I choose?

- I share **from my home computer or laptop**, only with family and friends → **Hidden**.
- I have **a VPS** and want **a private VPN** for my people or my customers → **Isolated**.
- I have a VPS but **cannot open a port** (or it sits behind someone else's router) → **Hidden**.
- I want **anybody** to be able to find and pay for my exit → **Public**.

## Step by step: share your home computer only with your people (desktop app)

1. Open **Proxy** in the app and scroll down to **Access to this computer**.
2. Choose **Hidden**. The card says that the new mode takes effect after a restart.
3. **Close the app completely and open it again.** The card now has a *hidden* label.
4. In **Share your connection** set your price (`0` means free for everybody who has the link) and
   press **Start sharing**. A hidden exit does not need the upload speed: you can leave it at 0.
   The card now says that the exit is not advertised.
5. Under **Access to this computer → Your invitation link**, press the copy button (or the QR-code
   button), and send the link to your people in a messenger you trust.

If the card says *This computer does not know an address others can reach it at yet*, wait a minute
until the app is connected to the network.

**Remember:** while your exit is on, the traffic of the people you invited leaves to the Internet
from **your** address. Read [What happens if I share my connection?](faq.md#what-happens-if-i-share-my-connection).

## Step by step: use a link somebody gave you (desktop app)

1. Copy the link they sent you (it starts with `echofog://join?`).
2. In the app open **Proxy → Specific node**, paste the link into the field.
3. Under the field the app shows what the link is: *Invitation to a hidden computer* and the first
   characters of its ID. If something is wrong, it says so (see [Problems](#problems-and-what-to-do)).
4. Press **Connect**.

The app remembers the key. Next time, use **Connect to the last exit** or pick it under **Your exits**: you
do not need to paste the link again. If the owner later makes a new link, you stay in.

Paying works as with any exit: if the owner set a price, the Proxy screen shows how to pay.

## Step by step: a private exit on a VPS with Docker (isolated)

This is the most common setup. It assumes you followed [VPS with Docker](deploy-vps.md) up to
step 3: Docker is installed, the port is open (UDP and TCP 443), and you have `deploy/.env`.

1. Open the settings file:

   ```bash
   nano deploy/.env
   ```

2. Set these lines (change the price to yours):

   ```bash
   ECHOFOG_ACCESS=isolated
   ECHOFOG_EXIT=true
   ECHOFOG_EXIT_PRICE=0.05        # dollars per GB; 0 = free for everybody with the link
   ECHOFOG_ADVERTISE_PORT=443     # already there: the port people connect to
   ```

   With `isolated` you do not need `ECHOFOG_BOOTSTRAP`, `ECHOFOG_RELAY` or
   `ECHOFOG_EXIT_UPLOAD_MBPS`: they are ignored.

3. Start (or restart) the node:

   ```bash
   docker compose -f deploy/docker-compose.yml up -d --build
   ```

4. Get the link:

   ```bash
   docker compose -f deploy/docker-compose.yml exec node echofog-node invite
   ```

   It prints one line starting with `echofog://join?`. That is the link to give out. The node also
   prints it in the log at start (`Invite link:`).

   People whose network blocks UDP need the TCP fallback, which in Docker stays off until you set a
   real site for the node to imitate (`ECHOFOG_TLS_SERVER_NAME`, `ECHOFOG_CAMOUFLAGE_ADDR`): see
   [TCP fallback](deploy-vps.md#tcp-fallback). UDP alone is enough for most people.

5. Check it from your own computer: paste the link into **Proxy → Specific node** and connect.
   Then, on the server, see who came in:

   ```bash
   docker compose -f deploy/docker-compose.yml exec node echofog-node invite --list
   ```

The link uses the server's public IP, which the node finds out by itself, with the port from
`ECHOFOG_ADVERTISE_PORT`. If you want the link to use a **domain name** instead (handy if the IP may
change), or the server has several addresses, add:

```bash
ECHOFOG_PUBLIC_ADDR=vpn.example.org:443
```

The key is created on the first start and kept in the `echofog-data` volume together with the node's
identity. It survives restarts and updates: the link stays the same until you make a new one.

## Step by step: a hidden node with Docker

The same as above, with `ECHOFOG_ACCESS=hidden`. Use it when the server cannot be reached from the
Internet (no open port, or it is behind a router): the node then keeps a connection to a couple of
relays and puts them in its link. Leave `ECHOFOG_BOOTSTRAP` as it is: a hidden node needs the network.

If the relays named in the link go away, people cannot reach a hidden node that has no open port.
Run `invite` again now and then, and give out the fresh link if it changed.

## Step by step: without Docker

```bash
echofog-node run --access isolated --exit --exit-price 0.05 --advertise-port 443
```

and in another terminal on the same server:

```bash
echofog-node invite
```

Every setting has a flag and an environment variable; see the table in
[Technical details](#technical-details).

## Managing who has access

All of this is on the **Access to this computer** card in the app, or the `invite` command on a
server (in Docker, prefix it with `docker compose -f deploy/docker-compose.yml exec node`).

| I want to… | In the app | On a server |
|---|---|---|
| See the link again | *Your invitation link* | `echofog-node invite` |
| See who came in | *Joined with the link* | `echofog-node invite --list` |
| Stop the link from working for new people | **New link** | `echofog-node invite --rotate` |
| Cut off one person | the bin button next to them | `echofog-node invite --remove <their peer ID>` |

How it works, in plain words:

- **Everybody who came in with the link is remembered.** They can reconnect any time without the
  link, even after you restart.
- **A new link** replaces the key. Whoever has the **old** link can no longer get in **for the
  first time**. People who already came in stay. This is what you do when a link leaked.
- **Removing a person** forgets them. To come back, they need the **current** link.
- So, to throw somebody out after a link leaked: make a new link, then remove that person, then send
  the new link to the people you still want.
- **Your contacts** get in without any link (so they can call you), and do not appear in the list.

## Money: the link is access, not free traffic

The link lets people **in**. They still pay the price you set, like on any exit; the first small
amount of test traffic each day is free when the exit is paid. To let somebody use your exit **for
free**, add them to **Free for these people** in the app (on a server: `ECHOFOG_EXIT_FREE_PEERS`).
To make it free for everybody with the link, set the price to 0.

## Keeping the link safe

- **Anybody who has the link can use your exit.** Send it only to people you trust, in a private
  chat. Do not post it publicly.
- If it leaked, make a **new link** (see above).
- The link does not contain your password, your recovery phrase or your wallet. It only opens the
  door to your node.
- On a server, the `invite` command only works on the server itself: nobody can ask your node for
  its link over the Internet.

### What the key actually protects

- A stranger who knows your address but not the key gets **no answer**: not a refusal, not your
  node's ID, nothing. Scanners looking for VPN servers see a silent UDP port, and on the TCP port the
  ordinary website the node imitates (see [Anti-DPI](anti-dpi.md)).
- A person with the key proves it **without sending the key itself**, and the proof only works for
  that one connection: somebody who records the traffic cannot reuse it.
- Even when a hidden node connects to other nodes itself (to relays, for example), they cannot use
  its exit: to them it looks switched off.

## Several servers behind one link

You can give several servers the **same key**, so one link works for all of them (each server still
has its own address in the link it prints). Make a key once:

```bash
docker compose -f deploy/docker-compose.yml run --rm node invite --new-secret
```

It prints 22 characters. Put them on every server in `deploy/.env`:

```bash
ECHOFOG_ACCESS_SECRET=<the 22 characters>
```

Or, to keep it out of the settings file, as a Docker secret: save it to `deploy/echofog_access_secret.txt`,
uncomment the `echofog_access_secret` lines in `deploy/docker-compose.yml`, and set
`ECHOFOG_ACCESS_SECRET_FILE=/run/secrets/echofog_access_secret`. Kubernetes secrets work the same
way, as a file.

A key set like this cannot be changed with `invite --rotate` or the **New link** button: change it
in the settings and restart the nodes.

## Problems and what to do

| What you see | What it means | What to do |
|---|---|---|
| *This link was made by a newer version of EchoFog* | The person who made the link has a newer app. | Update your app. |
| *This link is for another network (…)* | The link belongs to a different EchoFog-based network (a fork). | Ask for a link for this network, or use that network's app. |
| *This is not an invitation link…* | Part of the link is missing, often cut off by a messenger. | Ask the person to send it again, for example as a file or a QR code. |
| *This computer does not know an address others can reach it at yet* / `no link yet` | The node does not know its own public address yet. | Wait a minute. On a server, set `ECHOFOG_PUBLIC_ADDR`. |
| *Could not reach the node* | The address in the link does not answer. | Isolated: check that the server is running and UDP (and TCP) 443 is open in the firewall and in the provider's panel. Hidden: ask for a fresh link; its relays may have changed. |
| I switched to Hidden but nothing changed | The mode changes only after a restart. | Close the app completely and open it again. |
| My friends cannot call me since I went hidden | They should be able to, if they are in your contacts. | Check that they are in your chat list. |
| People got in before, now they cannot | Somebody removed them, or the node's data was lost (new identity). | Send them the current link. |

## Limits

- **Old versions of the app** do not know these modes: they cannot read invitation links, and a
  hidden node does not let them in.
- **Changing the mode takes a restart.** Isolated mode is only for servers; the app offers Public
  and Hidden.
- **Keep obfuscation on** (the default). With `--obfs off`, a scanner that completes the encrypted
  handshake with your port can learn your node's ID, though it still gets no further.
- **A hidden node behind a router** is reachable only through the relays named in its link. When it
  switches to other relays, give out a fresh link.
- Like every node, a hidden node answers other nodes' questions about **who else is on the network**,
  and may name the public addresses of nodes it knows, including people who use it. Its own address
  is never in those answers.

## Technical details

### The link

```
echofog://join?v=1&net=echofog&p=101&id=<peer id>&s=<secret>&a=<host:port>&r=<host:port/relay id>
```

| Part | Meaning |
|---|---|
| `v` | Link format. An app that cannot read it says "update the app" instead of failing silently. |
| `net` | The network: `echofog`, or a fork's own name, set at build time with `-ldflags "-X github.com/echofog/echofog-core/internal/config.NetworkName=myfork"`. A link of another network is refused with a clear message. |
| `p` | Protocol version the node speaks. |
| `id` | The node's peer ID. |
| `s` | The access secret, 16 bytes in base64url. Only in the link of a hidden or isolated node; a public node's link has none and only says where the node is. |
| `a` | An address to dial, `host:port` or a `ws://` URL. May repeat. |
| `r` | A relay the node is connected to, `host:port/<relay peer id>`, for a hole punch when no address answers. Hidden nodes only. May repeat. |

### How access is checked

- The initiator sends an HMAC-SHA256 of the secret over the connection's TLS channel binding, its
  nonce and its public key, inside the first handshake message. A hidden node checks it before
  sending anything, so a peer without it learns nothing, not even the node's public key.
- A hole punch request carries a ticket: an HMAC of the secret over the requester's peer ID and the
  current minute (±2 minutes). Without it a hidden node does not answer, so it gives its addresses to
  nobody else.
- The exit checks access on every tunnel too, including over connections the hidden node opened
  itself.
- The secret, the list of peers who joined and the secrets of nodes this node was invited to are
  kept in the encrypted part of the node's database. The joined list holds at most 10,000 peers.

### Server settings

| Flag | Environment | What it does |
|---|---|---|
| `--access` | `ECHOFOG_ACCESS` | `public` (default), `hidden` or `isolated`. Hidden and isolated turn the relay off. |
| `--access-secret` | `ECHOFOG_ACCESS_SECRET` | The secret, 22 characters of base64url. Empty: generated on the first start and kept in the data directory. |
| `--access-secret-file` | `ECHOFOG_ACCESS_SECRET_FILE` | Read the secret from a file (Docker or Kubernetes secret) when `--access-secret` is empty. |
| `--public-addr` | `ECHOFOG_PUBLIC_ADDR` | Addresses for the link, comma-separated `host:port` or `ws://` URLs, instead of the observed one. |
| `--advertise-port` | `ECHOFOG_ADVERTISE_PORT` | When set, the observed IP goes into the link with this port (Docker publishing host 443 to the container's 4242). |
| `--use-exit` | `ECHOFOG_USE_EXIT` | Also takes an invitation link, to use a hidden exit from a server. Quote it: `--use-exit '<link>'`. |

`echofog-node invite` prints the link; `--list`, `--rotate`, `--remove <peer id>` manage access;
`--new-secret` prints a fresh secret without a running node. They talk to the node's local status
endpoint (`--status-addr`, loopback by default), so they work only on the machine the node runs on,
and the commands that change something need a header a web page cannot send.
