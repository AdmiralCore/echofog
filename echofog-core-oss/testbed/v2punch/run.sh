#!/bin/sh
# v2 birthday lab (T6.7): a1 (cone) reaches b1 (symmetric, ports from one block) through the relay r1
# and waits for a direct path. Run from echofog-core-oss, as root, with a Docker that can run the
# lab (WSL Ubuntu: `wsl -d Ubuntu -u root`). Only one lab per machine: take locks/heavy-*.lock first.
#
#   sh testbed/v2punch/run.sh            # build, run, print the verdict, clean up
#   ROUNDS=3 NAT_B=symmetric-narrow PORTBLOCK_B=40000-40999 sh testbed/v2punch/run.sh
#
# Exit status 0: at least one round made a direct path through a birthday punch.
set -u
cd "$(dirname "$0")/../.." || exit 2
COMPOSE="docker compose -p efpunch -f testbed/docker-compose.v2punch.yml"
ROUNDS="${ROUNDS:-3}"
WAIT="${WAIT:-45s}"
export NAT_B="${NAT_B:-symmetric-narrow}" PORTBLOCK_B="${PORTBLOCK_B:-40000-40999}"
case "$PORTBLOCK_B" in
*-*) export PORTBLOCK_B_RANGE="$PORTBLOCK_B" ;;
esac

cleanup() { $COMPOSE down -v --remove-orphans >/dev/null 2>&1; }
trap cleanup EXIT

[ "${NO_BUILD:-}" = 1 ] || $COMPOSE build || exit 2
cleanup
$COMPOSE up -d || exit 2

# wait for the cards
i=0
until docker exec efpunch-b1 test -f /shared/b1.json 2>/dev/null; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "b1 never published its card"; $COMPOSE logs --no-color | tail -40; exit 2; }
  sleep 1
done
sleep 3 # b1 joins r1

ok=0
round=1
while [ "$round" -le "$ROUNDS" ]; do
  echo "== round $round: a1 dials b1 through r1"
  out=$(docker exec efpunch-a1 efv2 dial --data /data --listen noise3=10.20.1.11:4433 --via /shared/r1.json --wait-direct "$WAIT" --timeout 60s /shared/b1.json 2>&1)
  echo "$out"
  case "$out" in
  *'"direct": true'*) ok=$((ok + 1)) ;;
  esac
  round=$((round + 1))
done
echo "== counters on b1 (the symmetric side)"
docker exec efpunch-b1 sh -c 'cat /data/efv2-status.json 2>/dev/null | grep -E "birthday_(tried|ok)"; ls /data | head' 2>&1
echo "== b1 log tail"
docker exec efpunch-b1 sh -c 'tail -15 /tmp/run.log' 2>&1
echo "== verdict: $ok of $ROUNDS rounds reached a direct path"
[ "$ok" -ge 1 ]
