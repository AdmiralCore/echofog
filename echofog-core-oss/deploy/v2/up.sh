#!/bin/sh
# EchoFog v2: bring a server node up with one command.
#
# The script writes deploy/v2/.env (mode 0600) with only the values that differ from .env.example,
# writes the compose override deploy/v2/docker-compose.up.yml (container name, mounts, the extra
# invitation port), pulls the image, starts the node, waits until the image's healthcheck reports
# healthy and then prints `v2 status --plain`, the path of the recovery phrase and, only when
# invitations are on, a ready invitation command. An existing .env is kept as .env.bak.<date>
# before it is replaced. It downloads no scripts and pipes nothing into a shell; Docker is the only
# program it runs, and ECHOFOG_DOCKER names another binary that speaks the compose plugin.
#
# The profile is created at the first start: --invites, --cover/--site and ECHOFOG_ROLES shape it
# then, and a later run on the same volume does not change it. To apply new options, start over on
# an empty volume.
#
#   ./deploy/v2/up.sh --advertise 203.0.113.10
#   ./deploy/v2/up.sh --advertise 203.0.113.10 --invites
#   ./deploy/v2/up.sh --advertise 203.0.113.10 --cover node.example.org:443 --site /srv/echofog-site
#
# Options:
#   --advertise IP     the public IPv4 or IPv6 literal of this server (required, a name is refused)
#   --image TAG|REF    the image, a full reference or a tag (default: v2-dev)
#   --port N           the public port (default: 443); noise3 takes N+1
#   --invites          open the invitation listener: N+1, or noise3+1 with a cover
#   --cover HOST:PORT  the operator's own domain and port; needs --site
#   --site DIR         the folder with that site, mounted read-only at /srv/site
#   --test             a throwaway run: name ef-test, volume ef-test-data, ports from 8443/8444,
#                      then the commands that remove everything are printed
#   --name NAME        container and compose project name (default: echofog-v2-node in project v2)
#   --dry-run          print every command and the two generated files, change and run nothing
#
# Ports are not opened here: 443/tcp and 443/udp (444/udp for noise3, 445/udp with a cover and
# invitations) belong in the firewall and in the provider's panel. UDP matters as much as TCP.
set -eu
umask 077

die() {
	printf 'up.sh: %s\n' "$*" >&2
	exit 1
}

ADVERTISE=''
IMAGE='ghcr.io/admiralcore/echofog-node:v2-dev'
PORT=443
PORT_SET=0
INVITES=0
if [ "${ECHOFOG_INVITES:-false}" = "true" ]; then
	INVITES=1
fi
COVER=''
SITE=''
TEST=0
NAME=''
NAME_SET=0
DRY=0
DOCKER="${ECHOFOG_DOCKER:-docker}"
TIMEOUT="${ECHOFOG_UP_TIMEOUT:-180}"

while [ $# -gt 0 ]; do
	arg=$1
	shift
	case "$arg" in
	--advertise=*) ADVERTISE=${arg#*=} ;;
	--advertise)
		[ $# -ge 1 ] || die "--advertise needs the public IP literal of this server"
		ADVERTISE=$1
		shift
		;;
	--image=*) IMAGE=${arg#*=} ;;
	--image)
		[ $# -ge 1 ] || die "--image needs a tag or a full image reference"
		IMAGE=$1
		shift
		;;
	--port=*) PORT=${arg#*=}; PORT_SET=1 ;;
	--port)
		[ $# -ge 1 ] || die "--port needs a number"
		PORT=$1
		PORT_SET=1
		shift
		;;
	--invites) INVITES=1 ;;
	--cover=*) COVER=${arg#*=} ;;
	--cover)
		[ $# -ge 1 ] || die "--cover needs HOST:PORT"
		COVER=$1
		shift
		;;
	--site=*) SITE=${arg#*=} ;;
	--site)
		[ $# -ge 1 ] || die "--site needs the folder of the site"
		SITE=$1
		shift
		;;
	--test) TEST=1 ;;
	--name=*) NAME=${arg#*=}; NAME_SET=1 ;;
	--name)
		[ $# -ge 1 ] || die "--name needs a container name"
		NAME=$1
		NAME_SET=1
		shift
		;;
	--dry-run) DRY=1 ;;
	--help | -h)
		sed -n '2,/^set -eu$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown option $arg (see --help)" ;;
	esac
done

# is_ipv4 accepts four decimal octets, no leading zeros, each at most 255.
is_ipv4() {
	addr=$1
	case "$addr" in *[!0-9.]* | *..* | .* | *.) return 1 ;; esac
	oldifs=$IFS
	IFS=.
	# shellcheck disable=SC2086
	set -- $addr
	IFS=$oldifs
	[ $# -eq 4 ] || return 1
	for octet in "$1" "$2" "$3" "$4"; do
		case "$octet" in '' | 0?*) return 1 ;; esac
		[ "${#octet}" -le 3 ] || return 1
		[ "$octet" -le 255 ] || return 1
	done
}

# is_ipv6 accepts an IPv6 literal: hex groups of 1 to 4 digits, at most one "::", no dotted IPv4
# tail. A wrong group count would only surface after the healthcheck, 180 seconds later.
is_ipv6() {
	case "$1" in *:*) ;; *) return 1 ;; esac
	case "$1" in *[!0-9a-fA-F:]*) return 1 ;; esac
	[ "${#1}" -le 39 ] || return 1
	case "$1" in *:::* ) return 1 ;; esac
	compressed=0
	case "$1" in
	*::*)
		compressed=1
		rest=${1#*::}
		case "$rest" in *::*) return 1 ;; esac
		;;
	esac
	printf '%s\n' "$1" | awk -v compressed="$compressed" -F: '
	{
		groups = 0
		for (i = 1; i <= NF; i++) {
			if ($i == "") continue
			if (length($i) > 4 || $i !~ /^[0-9a-fA-F]+$/) exit 1
			groups++
		}
		if (compressed == 1 ? groups > 7 : groups != 8) exit 1
	}'
}

# valid_port accepts 1-65535 without leading zeros.
valid_port() {
	case "$1" in '' | *[!0-9]* | 0*) return 1 ;; esac
	[ "${#1}" -le 5 ] || return 1
	[ "$1" -le 65535 ] || return 1
}

valid_cover() {
	case "$1" in *:*) ;; *) return 1 ;; esac
	chost=${1%:*}
	cport=${1##*:}
	[ -n "$chost" ] || return 1
	valid_port "$cport" || return 1
	case "$chost" in *[!A-Za-z0-9.-]*) return 1 ;; esac
}

[ -n "$ADVERTISE" ] || die "--advertise is required: the public IPv4 or IPv6 literal of this server"
if ! is_ipv4 "$ADVERTISE" && ! is_ipv6 "$ADVERTISE"; then
	die "--advertise $ADVERTISE is not an IP literal: use the address, a name belongs in --cover"
fi
valid_port "$PORT" || die "--port $PORT is not a port number"
case "$TIMEOUT" in '' | *[!0-9]*) die "ECHOFOG_UP_TIMEOUT must be a number of seconds" ;; esac
[ "$TIMEOUT" -ge 10 ] || die "ECHOFOG_UP_TIMEOUT is too short"

case "$IMAGE" in
*/*) ;;
*) IMAGE="ghcr.io/admiralcore/echofog-node:$IMAGE" ;;
esac
case "$IMAGE" in *[!A-Za-z0-9._:/@-]* | '') die "--image $IMAGE is not an image reference" ;; esac

if [ -n "$COVER" ]; then
	valid_cover "$COVER" || die "--cover $COVER is not HOST:PORT (a name and a port)"
	[ -n "$SITE" ] || die "--cover needs --site: the node serves the operator's own site, never a foreign one"
fi
if [ -n "$SITE" ]; then
	[ -n "$COVER" ] || die "--site needs --cover: the site is served as the cover of a domain"
	case "$SITE" in /*) ;; *) die "--site $SITE must be an absolute path on this server" ;; esac
	case "$SITE" in
	*'
'*) die "--site: a newline is not a path" ;;
	esac
	if printf '%s' "$SITE" | grep -q '["$\\]'; then
		die "--site $SITE has a character that the compose file cannot carry"
	fi
	[ -d "$SITE" ] || die "--site $SITE is not a directory"
	[ -r "$SITE" ] || die "--site $SITE is not readable by this user"
fi
if [ -n "$NAME" ]; then
	case "$NAME" in
	'' | [!A-Za-z0-9]* | *[!A-Za-z0-9_.-]*) die "--name $NAME: letters, digits, dot, dash and underscore only" ;;
	esac
	[ "${#NAME}" -le 63 ] || die "--name $NAME is longer than 63 characters"
fi

# udp_port_busy reads the kernel tables: no socket is opened and no tool outside the base system is
# needed. A missing table (not Linux) means free.
udp_port_busy() {
	port=$1
	hex=$(printf '%04x' "$port" | tr 'abcdef' 'ABCDEF')
	for table in /proc/net/udp /proc/net/udp6; do
		[ -r "$table" ] || continue
		while IFS= read -r line; do
			case "$line" in *:"$hex"\ *) return 0 ;; esac
		done <"$table"
	done
	return 1
}

# pick_test_port returns the first pair of free UDP ports from 8443 up.
pick_test_port() {
	p=8443
	while [ "$p" -le 65000 ]; do
		if ! udp_port_busy "$p" && ! udp_port_busy "$((p + 1))"; then
			printf '%s\n' "$p"
			return 0
		fi
		p=$((p + 2))
	done
	die "no free pair of UDP ports from 8443: pass --port N"
}

if [ "$TEST" = 1 ]; then
	if [ -z "$NAME" ]; then
		NAME=ef-test
	fi
	if [ "$PORT_SET" = 0 ]; then
		PORT=$(pick_test_port)
	fi
	VOLUME=ef-test-data
else
	if [ -z "$NAME" ]; then
		NAME=echofog-v2-node
	fi
	VOLUME=''
fi
if [ "$NAME_SET" = 1 ]; then
	PROJECT=$NAME
elif [ "$TEST" = 1 ]; then
	PROJECT=ef-test
else
	PROJECT=v2
fi

NOISE_PORT=$((PORT + 1))
if [ -n "$COVER" ]; then
	INVITE_PORT=$((NOISE_PORT + 1))
else
	INVITE_PORT=$((PORT + 1))
fi
[ "$NOISE_PORT" -le 65535 ] || die "--port $PORT puts the noise3 listener on $NOISE_PORT: ports stop at 65535"
if [ -n "$COVER" ] && [ "$INVITES" = 1 ]; then
	[ "$INVITE_PORT" -le 65535 ] || die "--port $PORT with --cover and --invites puts the invitation listener on $INVITE_PORT: ports stop at 65535"
fi

SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
ROOT=$(CDPATH= cd "$SCRIPT_DIR/../.." && pwd)
cd "$ROOT"
COMPOSE=deploy/v2/docker-compose.yml
OVERRIDE=deploy/v2/docker-compose.up.yml
ENVFILE=deploy/v2/.env

ENV_PREEXISTED=0
if [ -f "$ENVFILE" ]; then
	ENV_PREEXISTED=1
	existing=$(sed -n 's/^ECHOFOG_ADVERTISE_ADDR=//p' "$ENVFILE" | head -n 1)
	if [ -n "$existing" ] && [ "$existing" != "$ADVERTISE" ]; then
		die "$ENVFILE already names $existing: move it away or rerun with --advertise $existing"
	fi
fi

# env_body prints the .env: only the values that differ from .env.example, plus the address that
# every operator must see.
env_body() {
	printf '# Generated by deploy/v2/up.sh: only the values that differ from .env.example.\n'
	if [ "$IMAGE" != 'ghcr.io/admiralcore/echofog-node:v2-dev' ]; then
		printf 'ECHOFOG_IMAGE=%s\n' "$IMAGE"
	fi
	printf 'ECHOFOG_ADVERTISE_ADDR=%s\n' "$ADVERTISE"
	if [ "$PORT" != 443 ]; then
		printf 'ECHOFOG_PORT=%s\n' "$PORT"
	fi
	if [ "$NOISE_PORT" != 444 ]; then
		printf 'ECHOFOG_NOISE_PORT=%s\n' "$NOISE_PORT"
	fi
	if [ "$INVITES" = 1 ]; then
		printf 'ECHOFOG_INVITES=true\n'
	fi
	if [ -n "$COVER" ]; then
		printf 'ECHOFOG_COVER=%s\n' "$COVER"
		printf 'ECHOFOG_SITE_DIR=/srv/site\n'
	fi
	return 0
}

# override_body prints the compose override: the container name, the data volume of a test run, the
# read-only site mount and the one port the base file does not publish.
override_body() {
	printf '# Generated by deploy/v2/up.sh: container name, mounts and ports. Rerun the script.\n'
	printf 'services:\n  node:\n    container_name: %s\n' "$NAME"
	if [ -n "$VOLUME" ] || [ -n "$SITE" ]; then
		printf '    volumes:\n'
		if [ -n "$VOLUME" ]; then
			printf '      - %s:/data\n' "$VOLUME"
		fi
		if [ -n "$SITE" ]; then
			printf '      - "%s:/srv/site:ro"\n' "$SITE"
		fi
	fi
	if [ -n "$COVER" ] && [ "$INVITES" = 1 ]; then
		printf '    ports:\n      - "%s:%s/udp"\n' "$INVITE_PORT" "$INVITE_PORT"
	fi
	if [ -n "$VOLUME" ]; then
		printf 'volumes:\n  %s:\n    name: %s\n' "$VOLUME" "$VOLUME"
	fi
	return 0
}

TMP=''
cleanup() {
	if [ -n "$TMP" ]; then
		rm -f "$TMP"
	fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

write_file() {
	TMP="$1.tmp.$$"
	"$2" >"$TMP"
	chmod 0600 "$TMP"
	mv "$TMP" "$1"
	TMP=''
}

plan_file() {
	printf '[dry-run] write %s (mode 0600)\n' "$1"
	printf '# --- %s begin ---\n' "$1"
	"$2"
	printf '# --- %s end ---\n' "$1"
}

# backup_env keeps an existing .env next to it before the new one replaces it: the operator's edits
# (roles, ACME, a certificate path) must survive a rerun with the same address.
backup_env() {
	[ "$ENV_PREEXISTED" = 1 ] || return 0
	backup="$ENVFILE.bak.$(date -u +%Y%m%dT%H%M%SZ)"
	cp -p "$ENVFILE" "$backup" || die "cannot keep the previous $ENVFILE as $backup"
	printf 'up.sh: kept the previous %s as %s\n' "$ENVFILE" "$backup"
}

# profile_note says what a rerun cannot change: the listeners and roles are written into the profile
# on the first start and read from it afterwards.
profile_note() {
	printf '\nOptions such as --invites and --cover/--site, and ECHOFOG_ROLES, shape the profile at the first start.\n'
	printf 'A node that already has one keeps it: to apply new options, start over on an empty volume.\n'
}

# dead prints the compose command as it would be typed.
dead() {
	printf '%s compose -p %s -f %s -f %s --env-file %s %s' \
		"$DOCKER" "$PROJECT" "$COMPOSE" "$OVERRIDE" "$ENVFILE" "$*"
}

compose() {
	if [ "$DRY" = 1 ]; then
		printf '[dry-run] %s\n' "$(dead "$@")"
		return 0
	fi
	printf '+ %s\n' "$(dead "$@")"
	"$DOCKER" compose -p "$PROJECT" -f "$COMPOSE" -f "$OVERRIDE" --env-file "$ENVFILE" "$@"
}

wait_healthy() {
	waited=0
	while :; do
		state=$("$DOCKER" inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$NAME" 2>/dev/null || true)
		case "$state" in
		healthy) return 0 ;;
		unhealthy)
			compose logs --tail=50 node || true
			die "the node is unhealthy: the container runs but its operator API does not answer. The log above says why"
			;;
		esac
		if [ "$waited" -ge "$TIMEOUT" ]; then
			compose ps || true
			compose logs --tail=50 node || true
			die "the node did not become healthy in ${TIMEOUT}s (last state: ${state:-none}). The log above says why: a port held by another process, a missing ECHOFOG_ADVERTISE_ADDR or a failed pull are the usual causes"
		fi
		sleep 3
		waited=$((waited + 3))
	done
}

teardown() {
	printf 'Teardown of the test run (removes the container and the volume):\n'
	printf '  %s\n' "$(dead down -v)"
	printf '  docker volume rm -f %s\n' "$VOLUME"
	if [ "$ENV_PREEXISTED" = 0 ]; then
		printf '  rm -f %s %s\n' "$ENVFILE" "$OVERRIDE"
	else
		printf '  %s was already there and now holds the test values: restore or remove it by hand.\n' "$ENVFILE"
		printf '  rm -f %s\n' "$OVERRIDE"
	fi
}

if [ "$DRY" = 1 ]; then
	if [ "$ENV_PREEXISTED" = 1 ]; then
		printf '[dry-run] keep the previous %s as %s.bak.<date>\n' "$ENVFILE" "$ENVFILE"
	fi
	plan_file "$ENVFILE" env_body
	plan_file "$OVERRIDE" override_body
	compose pull
	compose up -d
	compose exec -T node echofog-node v2 status --data /data --plain
	compose exec -T node cat /data/recovery-phrase.txt
	if [ "$INVITES" = 1 ]; then
		compose exec -T node echofog-node v2 invite create --data /data --access 30d --out /data/invite.txt
		compose exec -T node cat /data/invite.txt
	else
		printf '[dry-run] invitations are off: the node starts without --invites\n'
	fi
	profile_note
	if [ "$TEST" = 1 ]; then
		teardown
	fi
	exit 0
fi

backup_env
write_file "$ENVFILE" env_body
write_file "$OVERRIDE" override_body
printf 'up.sh: wrote %s (0600) and %s\n' "$ENVFILE" "$OVERRIDE"

compose pull
compose up -d
wait_healthy

printf '\nEchoFog v2 is up: %s (%s).\n\n' "$NAME" "$IMAGE"
compose exec -T node echofog-node v2 status --data /data --plain

printf '\nRecovery phrase: /data/recovery-phrase.txt inside the volume, never in the log.\n'
printf 'Copy it off the server, then delete it:\n'
printf '  %s\n' "$(dead exec -T node cat /data/recovery-phrase.txt)"
printf '  %s\n' "$(dead exec -T node rm /data/recovery-phrase.txt)"

if [ "$INVITES" = 1 ]; then
	printf '\nInvitation (the link is written where only the node user can read it):\n'
	printf '  %s\n' "$(dead exec -T node echofog-node v2 invite create --data /data --access 30d --out /data/invite.txt)"
	printf '  %s\n' "$(dead exec -T node cat /data/invite.txt)"
else
	printf '\nInvitations are off: the node was started without --invites.\n'
	printf 'For invitation links, add --invites and start again on a fresh volume: the listener is set only at the first start.\n'
fi

profile_note

printf '\nPorts %s/tcp and %s/udp' "$PORT" "$PORT"
if [ -n "$COVER" ]; then
	printf ' (noise3 %s/udp' "$NOISE_PORT"
	if [ "$INVITES" = 1 ]; then
		printf ', invitations %s/udp' "$INVITE_PORT"
	fi
	printf ')'
elif [ "$INVITES" = 1 ]; then
	printf ' (invitations %s/udp)' "$INVITE_PORT"
fi
printf ' must be open in the firewall and in the provider panel; UDP matters as much as TCP.\n'

if [ "$TEST" = 1 ]; then
	printf '\nThe test run writes the same %s as a real node: do not start it next to one.\n' "$ENVFILE"
	teardown
fi
