#!/bin/sh
# Brings the v2 punch lab up and runs one dial with the logs of both sides (debugging aid).
cd "$(dirname "$0")/../.." || exit 2
C="docker compose -p efpunch -f testbed/docker-compose.v2punch.yml"
[ "${REBUILD:-}" = 1 ] && $C build 2>&1 | tail -2
$C down -v >/dev/null 2>&1
$C up -d >/dev/null 2>&1
sleep "${SETTLE:-14}"
docker exec efpunch-a1 efv2 dial --data /data --listen noise3=10.20.1.11:4433 --via /shared/r1.json --wait-direct "${WAIT:-15s}" --timeout 40s /shared/b1.json 2>&1 | grep -E '"direct"|birthday'
echo "--- b1"
docker exec efpunch-b1 sh -c 'grep -E birthday /data/efv2-status.json'
echo "--- r1"
docker exec efpunch-r1 sh -c 'tail -5 /tmp/run.log'
[ "${KEEP:-}" = 1 ] || $C down -v >/dev/null 2>&1
