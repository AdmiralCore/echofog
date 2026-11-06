#!/bin/sh
# Starts the v2 node. On the first start it creates the profile in $ECHOFOG_DATA_DIR; the recovery
# phrase that "init" prints goes to a private file, never to the container log (logs are copied to
# places the data volume is not). Any other command line is passed to echofog-node as it is:
#   docker compose run --rm node v2 invite create --data /data --access 30d
set -eu
umask 077

DATA="${ECHOFOG_DATA_DIR:-/data}"

if [ "${1:-}" != "v2" ] || [ "${2:-}" != "run" ]; then
	exec echofog-node "$@"
fi

if [ ! -f "$DATA/server-identity.json" ]; then
	ADDR="${ECHOFOG_ADVERTISE_ADDR:-}"
	if [ -z "$ADDR" ]; then
		echo "ECHOFOG_ADVERTISE_ADDR is required on the first start: the public IP address of this server (see deploy/v2/.env.example)" >&2
		exit 1
	fi
	case "$ADDR" in
	*[!0-9a-fA-F:.]*)
		echo "ECHOFOG_ADVERTISE_ADDR must be an IPv4 or IPv6 literal, not a name" >&2
		exit 1
		;;
	esac
	PORT="${ECHOFOG_PORT:-443}"
	PHRASE_FILE="$DATA/recovery-phrase.txt"
	set -- init --data "$DATA" --listen "noise3=0.0.0.0:$PORT" --advertise "$ADDR" --roles "${ECHOFOG_ROLES-relay,mailbox,dht-server}"
	if [ "${ECHOFOG_INVITES:-false}" = "true" ]; then
		set -- "$@" --invite-listen "noise3=0.0.0.0:$((PORT + 1))"
	fi
	if [ -n "${ECHOFOG_COVER:-}" ]; then
		set -- "$@" --cover "$ECHOFOG_COVER"
	fi
	echofog-node v2 "$@" > "$PHRASE_FILE"
	echo "echofog-node: profile created. The recovery phrase is in $PHRASE_FILE inside the data volume: copy it somewhere safe, then delete the file." >&2
fi

exec echofog-node v2 run --data "$DATA"
