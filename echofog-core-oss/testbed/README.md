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
```

The runner starts everything, checks and stops everything. If a check fails the containers stay
up for a look (`docker logs efbed-a1`), and the seed to repeat the run is printed.

**smoke** checks that all ten nodes find each other; that every node can write to every other node
(90 messages, most to somebody it has never talked to) and each arrives exactly once; that a group
of all ten forms, and that a message from each member reaches each member.

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
