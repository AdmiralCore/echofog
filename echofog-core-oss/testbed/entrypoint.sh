#!/bin/sh
# One entrypoint for every container of the test bed:
#   ROLE=gateway  a NAT router between a private network and the "Internet" network
#   ROLE=node     a node; with GATEWAY set, all its traffic leaves through that router
#   ROLE=efv2     an efv2 node of the v2 scenarios: EFV2_MODE=serve|idle, EFV2_LISTEN, EFV2_PROFILE
set -eu

case "${ROLE:-node}" in
gateway)
  # the interface that sits in the WAN subnet is the outside, any other one is the inside
  WAN_NET="${WAN_NET:-203.0.113.}"
  WAN_IF=$(ip -o -4 addr show | awk -v p="$WAN_NET" '$4 ~ "^"p {print $2; exit}')
  LAN_IF=$(ip -o -4 addr show | awk -v w="$WAN_IF" '$2 != "lo" && $2 != w {print $2; exit}')
  echo "gateway ${NAT:-cone}: wan=$WAN_IF lan=$LAN_IF netem='${NETEM:-}'"

  # What reaches the router's own outside address from nowhere is dropped before it can leave a trace
  # in the connection table: a stray packet that did would later make the router pick another outside
  # port for a node's flow to the same peer, and a hole punch aims at the port the node had.
  iptables -A INPUT -i "$WAN_IF" -m conntrack --ctstate NEW -j DROP
  iptables -P FORWARD DROP
  iptables -A FORWARD -i "$LAN_IF" -o "$WAN_IF" -j ACCEPT
  iptables -A FORWARD -i "$WAN_IF" -o "$LAN_IF" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
  case "${NAT:-cone}" in
  # port-restricted cone: one outside port per inside socket, answers only from who was written to
  cone) iptables -t nat -A POSTROUTING -o "$WAN_IF" -j MASQUERADE ;;
  # symmetric: a new outside port for every destination, drawn at random from the whole range
  # (--random-fully uses the kernel's full-range PRNG, the hardest case for a birthday punch)
  symmetric) iptables -t nat -A POSTROUTING -o "$WAN_IF" -j MASQUERADE --random-fully ;;
  # symmetric-narrow: a symmetric NAT whose outside ports come from a small block, as a carrier's
  # CGNAT hands each subscriber. This is the case a birthday punch is meant for: the peer aims its
  # spray at the block (PORTBLOCK, e.g. 40000-44000). Still a new port per destination, so the
  # ordinary punch cannot reach it.
  symmetric-narrow)
    # --to-ports needs a protocol, so UDP and TCP are restricted to the block and the rest (ICMP etc.)
    # keeps a plain random mapping
    iptables -t nat -A POSTROUTING -o "$WAN_IF" -p udp -j MASQUERADE --random-fully --to-ports "${PORTBLOCK:-40000-44000}"
    iptables -t nat -A POSTROUTING -o "$WAN_IF" -p tcp -j MASQUERADE --random-fully --to-ports "${PORTBLOCK:-40000-44000}"
    iptables -t nat -A POSTROUTING -o "$WAN_IF" -j MASQUERADE --random-fully
    ;;
  *) echo "unknown NAT '${NAT}'" >&2; exit 1 ;;
  esac

  if [ -n "${NETEM:-}" ]; then
    # shellcheck disable=SC2086
    tc qdisc replace dev "$WAN_IF" root netem $NETEM
  fi
  echo "$WAN_IF" > /tmp/wan_if
  exec sleep infinity
  ;;

web)
  exec testnode -web ":80"
  ;;

# a node of the v2 scenarios (T15.1): the profile is created, then the node either serves or waits
# for `docker exec efbed-<name> efv2 ...` from the runner
efv2)
  if [ -n "${GATEWAY:-}" ]; then
    ip route replace default via "$GATEWAY"
  fi
  efv2 init --data /data --profile "${EFV2_PROFILE:-desktop}" >/tmp/init.log 2>&1
  case "${EFV2_MODE:-idle}" in
  serve) exec efv2 run --data /data --listen "${EFV2_LISTEN:?EFV2_LISTEN is required with EFV2_MODE=serve}" ;;
  idle) exec sleep infinity ;;
  *) echo "unknown EFV2_MODE '${EFV2_MODE}'" >&2; exit 1 ;;
  esac
  ;;

node)
  if [ -n "${GATEWAY:-}" ]; then
    ip route replace default via "$GATEWAY"
  fi
  exec testnode \
    -name "${NAME:-node}" \
    -listen "${LISTEN:-:4242}" \
    -http ":8080" \
    -bootstrap "${BOOTSTRAP:-}" \
    -transport "${TRANSPORT:-}"
  ;;
esac
