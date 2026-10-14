#!/bin/sh
# One entrypoint for every container of the test bed:
#   ROLE=gateway  a NAT router between a private network and the "Internet" network
#   ROLE=node     a node; with GATEWAY set, all its traffic leaves through that router
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
  # symmetric: a new, random outside port for every destination
  symmetric) iptables -t nat -A POSTROUTING -o "$WAN_IF" -j MASQUERADE --random ;;
  *) echo "unknown NAT '${NAT}'" >&2; exit 1 ;;
  esac

  if [ -n "${NETEM:-}" ]; then
    # shellcheck disable=SC2086
    tc qdisc replace dev "$WAN_IF" root netem $NETEM
  fi
  echo "$WAN_IF" > /tmp/wan_if
  exec sleep infinity
  ;;

node)
  if [ -n "${GATEWAY:-}" ]; then
    ip route replace default via "$GATEWAY"
  fi
  exec testnode \
    -name "${NAME:-node}" \
    -listen ":4242" \
    -http ":8080" \
    -bootstrap "${BOOTSTRAP:-}" \
    -transport "${TRANSPORT:-}"
  ;;
esac
