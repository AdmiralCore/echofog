Русская версия: [reachability.ru.md](reachability.ru.md)

# "This computer cannot be reached from the Internet"

You may see this on the **Relay** card in the **Proxy** screen. Short answer first: **chats, calls
and using the VPN all work anyway.** Being reachable only matters if you want to serve others as a
relay or a public exit. To share your connection with a few people of your own, you do not need it:
see [below](#sharing-with-your-own-people-without-an-open-port).

## What is going on

- **NAT** is your router sharing one public address among all your devices. Connections *you* open
  go out fine. But when someone on the Internet tries to connect *to* you, the router does not know
  which device to send it to, so it drops it.
- **CGNAT** (carrier-grade NAT) is the same thing one level up: your provider shares one public
  address among many customers. Your router itself has no public address. This is common on mobile
  internet and with some home providers.

## Why your own calls still work

EchoFog tries, in this order:

1. **A direct connection**, when one side is reachable.
2. **Hole punching:** with the help of signaling nodes, both sides send packets to each other at the
   same moment, and many routers then let the connection through.
3. **A relay:** if neither works (for example both people are on mobile internet), the call or
   message goes through a relay node that both can reach.

So being unreachable never blocks you from using EchoFog. It only means your computer cannot be
someone else's relay or public exit.

## Sharing with your own people without an open port

If you only want to share your connection with family or friends, switch **Proxy → Access to this
computer** to **Hidden** and give them your invitation link. A hidden exit keeps a connection to a
couple of relays and names them in its link, so the people you invited reach you through them (and
punch through to a direct connection when the routers allow), even if nothing here can be fixed.
Step by step: [Hidden and isolated nodes](hidden-nodes.md).

## Which case are you in

Open **Proxy** and look at the **Relay** card. Set the relay mode to **Public** or **Private** if it
is off. Under **Your relay link**:

- A link `echofog://relay/…` with *"This computer has a public address"* or *"Reachable through the
  port your router forwards"* — you are reachable. Nothing to do.
- *"Checking how this computer connects…"* — the app is still finding out. Wait a minute.
- *"This computer cannot be reached from the Internet"* — open **How to become reachable**. The app
  shows the steps that fit your case, in the right order:

| What the app found | What it means | Steps it suggests |
|---|---|---|
| Router does not forward a port | Ordinary home NAT | UPnP → forward a port → public IP → IPv6 → VPS |
| Carrier-grade or double NAT (CGNAT) | Your router has no public address; port forwarding cannot help | public IP → VPS → IPv6 |
| NAT that changes its address for every peer (symmetric NAT) | Hole punching rarely works | public IP → VPS → forward a port |

On a server running `echofog-node`, `echofog-node status` shows the same thing in its `NAT:` line
(`public`, `cone`, `symmetric` or `unknown`).

## How to fix it

### 1. Turn on UPnP or NAT-PMP on the router

In the router's settings, enable **UPnP** (or **NAT-PMP / PCP**). Then the app asks the router for a
port by itself. It retries every 15 minutes, or restart the app to try at once.

### 2. Forward a port by hand

1. On the **Relay** card, set **Listen port** to a fixed number (with `0` the app picks a new port
   at every start, so a forward would stop matching). Click **Save port** and restart the app.
2. In the router, forward that port — **both UDP and TCP** — to this computer's local address.
3. Restart the app and check the card again.

For `echofog-node`, the port is the one in `--listen-addr` (default `4242`).

### 3. Ask your provider for a public ("white") IP

If you are behind CGNAT, no port forward can help: the router you control is not the one facing the
Internet. Ask your provider for a public IPv4 address (often a paid option). A quick way to tell: if
the WAN address on your router's status page differs from the address websites show as yours, or it
starts with `100.64`–`100.127`, `10.`, `172.16`–`172.31` or `192.168.`, you are behind CGNAT.

### 4. IPv6 (honest status)

IPv6 usually has no NAT, so in principle it would solve this. Today EchoFog **accepts** IPv6
connections but does **not yet advertise or prefer** IPv6 addresses, so having IPv6 does not make
you reachable on its own. It is on the roadmap.

### 5. Run a node on a VPS

A cheap rented server with a public IP is reachable at once, with no router setup. Run
`echofog-node` there ([run-a-node.md](run-a-node.md)) and share its link, so your other devices and
your friends join through it.
