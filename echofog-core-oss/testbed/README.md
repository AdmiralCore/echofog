# Test bed

A small Internet in Docker, to test what unit tests cannot: nodes behind NAT, links that lose and
delay packets, nodes that crash, and the promise that a message the API accepted arrives.

```
        wan 203.0.113.0/24  ("the Internet")
   s1  s2  p1  p2   public nodes
   gw_a  gw_b  gw_c   routers (NAT, delay, loss)
    |     |     |
  lan_a lan_b lan_c
  a1 a2 b1 b2 c1 c2   nodes behind them
```

| Router | NAT | Direct connection to another LAN |
| --- | --- | --- |
| gw_a | port-restricted cone | with c: by hole punching; with b: never |
| gw_b | symmetric (new random port per destination) | never, relays only |
| gw_c | port-restricted cone | with a: by hole punching |

Needs Docker with Compose (Docker Desktop is enough, also on Windows). Nothing leaves the machine.

## Run

```bash
go run ./testbed/runner -scenario smoke          # ~3 minutes
go run ./testbed/runner -scenario chaos          # smoke, then 3 minutes of failures
go run ./testbed/runner -scenario chaos -chaos-for 10m -seed 42   # the same failures again
go run ./testbed/runner -scenario vpn            # ~8 minutes: people using an exit while it breaks
go run ./testbed/runner -scenario load           # ~3 minutes: many downloads at once through one exit
go run ./testbed/runner -scenario soak -soak-for 2h   # steady use for hours, graphing resources
go run ./testbed/runner -scenario hidden         # an exit for friends, reached only by its link
go run ./testbed/runner -scenario call           # a call across the three NATs, voice and video counted
go run ./testbed/runner -scenario censor         # how EchoFog's traffic looks to DPI at the border
```

**censor** records what leaves each router while people use the VPN over QUIC and over the TCP
fallback, write to each other, and one of them visits the cover site as a browser (the control).
`testbed/censor` reads each capture as a censor would: what protocol each flow shows, its SNI and
whether the address hosts that site, its ClientHello fingerprint (GREASE or not), whether a QUIC
packet is real QUIC (it decrypts the Initial with the keys of its own connection ID, as GFW and
TSPU do), and whether the first data would be blocked by the Great Firewall's "fully encrypted"
rule. With the `echofog-ndpi` image (build `testbed/ndpigen`) nDPI classifies the same captures.
The run prints a visibility score, the share of EchoFog's bytes a GFW-like censor could single
out, and writes the per-flow table to `censor-report.txt`; `-strict` fails while it is above 0.
`-censor-for 3m` also keeps two clients downloading through the exit for that long and reports the
shape of the flows: how many connections were both long and heavy (`-heavy-after`, 2 minutes, and
`-heavy-mb`, 20 MB, by default), which a browser never makes.

Every run keeps the logs of all containers in `testbed/out/<time>-<scenario>/`, with
`summary.txt`: crashes, errors and race reports found in them, how often each container started,
and the warnings that came most often. A client that fails a VPN check leaves `fail-*.log` there with
what it knew at that moment and its last 90 seconds of log. `LOG_LEVEL=debug` before the command
makes the nodes log everything.

The runner starts everything, checks and stops everything. If a check fails the containers stay
up for a look (`docker logs efbed-a1`), and the seed to repeat the run is printed.

**smoke** checks that all ten nodes find each other; that every node can write to every other node
(90 messages, most to somebody it has never talked to) and each arrives exactly once; that a group
of all ten forms, and that a message from each member reaches each member.

The VPN scenarios add two exits in the server role, as on a VPS (`x1`, `x2` at 203.0.113.15-16),
and a web server standing in for the Internet (`web`, 203.0.113.30), which serves files with their
SHA-256 so a client can tell a whole download from a damaged one. The six nodes behind the routers
are the people.

**vpn** connects everybody to `x1`, downloads 8 MB each and checks it, disconnects and connects
again `-cycles` times per client, cuts each router off for 20 s, kills `x1` (the clients must move
to `x2`), freezes the exit most clients use (they must move back to `x1`, which is up again by
then), puts 15 % loss and 150 ms delay on a router, lets one node fail handshakes with `x1` and then
connects its neighbour behind the same router, and places a call through the symmetric NAT. Each
time a client must work again within `-recover` (60 s); the report gives median, p90 and worst.

**load** runs 64, 16 and 256 downloads at once from every client through one exit and wants 99 %
of them whole; afterwards the exit must give its goroutines, sockets and memory back.

**soak** keeps people browsing and writing for `-soak-for`, breaks something every few minutes
(`-soak-chaos=false` to leave it in peace), writes goroutines, heap, open files and peers of every
node to `metrics.csv` every 30 s, and fails a node whose numbers grow from start to end.

**chaos** repeats that, and then, for a few minutes, writes to random nodes all the time while it
breaks things at random: `kill -9` and restart of a node, freezing a node (it stops answering but
does not die), cutting a router off the Internet, 10-40 % loss and 50-300 ms delay on a router.
When it heals everything the rule is: **every message that the API accepted must arrive exactly
once**, in direct chats and in the group.

## By hand

```bash
docker compose -f testbed/docker-compose.yml -p efbed up -d s1 s2 gw_a gw_b gw_c
# ... read the ids from `docker exec efbed-s1 curl -s localhost:8080/status`, then
BOOTSTRAP=203.0.113.11:4242/<id1>,203.0.113.12:4242/<id2> docker compose -f testbed/docker-compose.yml -p efbed up -d
docker exec efbed-a1 curl -s localhost:8080/status
docker exec efbed-gw_b tc qdisc replace dev eth1 root netem delay 200ms loss 20%   # hurt a router
docker exec efbed-gw_a iptables -I FORWARD 1 -j DROP                                  # cut it off
```

The control interface of every node (`testbed/testnode`): `/status`, `POST /send`, `/history?peer=`,
`/pending?peer=`, `/group/{create,list,invite,join,send,messages,members}`.

## Limits

It is one Linux kernel pretending to be a network: no real radio, no carrier-grade NAT, no real
censor. Full-cone NAT is not modelled (Linux does not do it with plain `MASQUERADE`). Use it to
find logic and recovery bugs; real NATs and real DPI still need real machines.
