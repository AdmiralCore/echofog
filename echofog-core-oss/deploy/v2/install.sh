#!/bin/sh
# EchoFog v2 server installer (bootstrap).
#
# It downloads the node binary and the signed release manifest over https into a private temporary
# folder and starts the node's own installer on them:  echofog-node v2 install
# The binary is NOT trusted because of this script. The installer inside it checks the binary against
# the manifest (two signatures of the three release keys of the build) and installs nothing, and runs
# no system command, unless that check passes. Nothing is sent anywhere but the two downloads.
#
#   ECHOFOG_RELEASE_URL=https://example.org/releases/v2 sh install.sh --preset relay --advertise 203.0.113.10
#
# Download this file, read it, then run it. Do not pipe a download into a shell. Every argument is
# passed on to "echofog-node v2 install" (see its help for the presets, --lang ru|en and --start).
set -eu
umask 077

if [ -z "${ECHOFOG_RELEASE_URL:-}" ]; then
	echo "ECHOFOG_RELEASE_URL is required: the https address of the release folder (it holds echofog-node-linux-amd64, echofog-node-linux-arm64 and manifest.bin)" >&2
	exit 1
fi
case "$ECHOFOG_RELEASE_URL" in
https://*) ;;
*)
	echo "ECHOFOG_RELEASE_URL must start with https" >&2
	exit 1
	;;
esac

if [ "$(uname -s)" != "Linux" ]; then
	echo "this installer is for Linux (systemd)" >&2
	exit 1
fi
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*)
	echo "unsupported processor: $(uname -m)" >&2
	exit 1
	;;
esac
if [ "$(id -u)" != "0" ]; then
	echo "run the installer as root (for example after su -): it creates a service user and a systemd unit" >&2
	exit 1
fi
command -v curl >/dev/null 2>&1 || {
	echo "curl is required" >&2
	exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM HUP
BIN="$WORK/echofog-node"

fetch() {
	curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --max-redirs 3 --max-filesize 268435456 -o "$2" "$1"
}
fetch "$ECHOFOG_RELEASE_URL/echofog-node-linux-$ARCH" "$BIN"
fetch "$ECHOFOG_RELEASE_URL/manifest.bin" "$WORK/manifest.bin"
chmod 0700 "$BIN"

"$BIN" v2 install --binary "$BIN" --manifest "$WORK/manifest.bin" --platform "linux-$ARCH" "$@"
