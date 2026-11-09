#!/bin/sh
# EchoFog v2 server installer (bootstrap).
#
# The trust root of the whole install path is the release public keys written below, between the
# BEGIN and END marks. The script downloads a signed text record of the release (release.txt), checks
# its signatures with the base-system tool "ssh-keygen -Y verify", and only then looks at the binary:
# the binary must have the size and SHA-256 of the record. The downloaded file is made executable and
# started only after that; a failed check deletes it and stops. The node's own installer
# (echofog-node v2 install) then checks the binary once more against the signed manifest.
#
# Two of the three release keys must sign the record (the same rule as the application's update).
# Nothing is sent anywhere except the downloads.
#
#   curl --proto '=https' --tlsv1.2 -fsSL -o install.sh https://example.org/install.sh
#   sha256sum install.sh               # compare with the site and the README, then:
#   ECHOFOG_RELEASE_URL=https://example.org/releases/v2 sh install.sh --preset relay --advertise 203.0.113.10
#
# Download this file, read it and compare the SHA-256 of the whole file (sha256sum install.sh) with
# the value published on the website and in the README. Do not pipe a download into a shell.
#
# Options of the script itself (every other argument goes to "echofog-node v2 install"):
#   --fingerprint     print the SHA-256 of this file and of the key block below, and exit (nothing is
#                     downloaded). It is not a proof: a changed script can print anything.
#   --allow-rollback  allow a version below the installed one (a warning is printed); a version below
#                     the signed min_version, or below the floor of this script, is never allowed
#
# ECHOFOG_INSTALLED_BIN names the installed node binary (default /usr/local/bin/echofog-node); its
# "version" is read to refuse a rollback. It is the operator's own file, started only for that.
set -eu
umask 077

# BEGIN RELEASE KEYS
RELEASE_KEYS='PLACEHOLDER'
KEY_EPOCH=0
# END RELEASE KEYS
MIN_VERSION_FLOOR=0.0.0
THRESHOLD=2
NS_RELEASE=echofog-release-v1
NS_ROTATION=echofog-rotation-v1

die() {
	echo "install.sh: $*" >&2
	exit 1
}

ALLOW_ROLLBACK=0
FINGERPRINT=0
n=$#
while [ "$n" -gt 0 ]; do
	a=$1
	shift
	n=$((n - 1))
	case "$a" in
	--fingerprint) FINGERPRINT=1 ;;
	--allow-rollback) ALLOW_ROLLBACK=1 ;;
	*) set -- "$@" "$a" ;;
	esac
done

if [ "$FINGERPRINT" = 1 ]; then
	[ -f "$0" ] || die "run the script from a file (sh install.sh --fingerprint), not from a pipe"
	command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
	echo "file $(sha256sum "$0" | cut -d' ' -f1)"
	echo "keys $(sed -n '/^# BEGIN RELEASE KEYS$/,/^# END RELEASE KEYS$/p' "$0" | sha256sum | cut -d' ' -f1)"
	echo "This is not a proof: a changed script can print anything. Compare the value of: sha256sum install.sh" >&2
	exit 0
fi

case "$RELEASE_KEYS" in
*PLACEHOLDER*) die "this file is a template: it has no release keys, so it cannot verify anything. Download install.sh from the release pages" ;;
esac

[ -n "${ECHOFOG_RELEASE_URL:-}" ] || die "ECHOFOG_RELEASE_URL is required: the https address of the release folder (it holds release.txt, release.txt.sigs, manifest.bin, echofog-node-linux-amd64 and echofog-node-linux-arm64)"
case "$ECHOFOG_RELEASE_URL" in
https://*) ;;
*) die "ECHOFOG_RELEASE_URL must start with https" ;;
esac

[ "$(uname -s)" = "Linux" ] || die "this installer is for Linux (systemd)"
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported processor: $(uname -m)" ;;
esac
[ "$(id -u)" = "0" ] || die "run the installer as root (for example after su -): it creates a service user and a systemd unit"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
command -v ssh-keygen >/dev/null 2>&1 || die "ssh-keygen is required (OpenSSH 8.0 or newer, package openssh-client): it verifies the release signature, and nothing is installed without that check"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
trap 'exit 1' INT TERM HUP
BIN="$WORK/echofog-node"
set -f

# fetch URL FILE [MAXBYTES]
fetch() {
	curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location --max-redirs 3 --connect-timeout 20 --max-time 900 --max-filesize "${3:-65536}" -o "$2" "$1"
}

# semver_ok X.Y.Z: exact shape, no leading zeros, at most six digits a part.
semver_ok() {
	case "$1" in *[!0-9.]* | '') return 1 ;; esac
	oldifs=$IFS
	IFS=.
	# shellcheck disable=SC2086
	set -- $1
	IFS=$oldifs
	[ $# -eq 3 ] || return 1
	for p in "$1" "$2" "$3"; do
		case "$p" in '' | 0?*) return 1 ;; esac
		[ "${#p}" -le 6 ] || return 1
	done
}

# semver_val X.Y.Z: a number that orders versions (the caller has checked the shape).
semver_val() {
	oldifs=$IFS
	IFS=.
	# shellcheck disable=SC2086
	set -- $1
	IFS=$oldifs
	echo $(($1 * 1000000000000 + $2 * 1000000 + $3))
}

# signers NAMESPACE DATAFILE SIGSFILE PREFIX: prints how many distinct release keys signed DATAFILE
# (SIGSFILE holds armored signatures one after the other).
signers() {
	ns=$1
	data=$2
	sigs=$3
	prefix=$4
	count=0
	blocks=0
	inblock=0
	while IFS= read -r line || [ -n "$line" ]; do
		if [ "$line" = "-----BEGIN SSH SIGNATURE-----" ]; then
			blocks=$((blocks + 1))
			[ "$blocks" -le 8 ] || break
			: >"$WORK/$prefix.sig.$blocks"
			inblock=1
		fi
		if [ "$inblock" = 1 ]; then
			printf '%s\n' "$line" >>"$WORK/$prefix.sig.$blocks"
		fi
		if [ "$line" = "-----END SSH SIGNATURE-----" ]; then
			inblock=0
		fi
	done <"$sigs"
	[ "$blocks" -le 8 ] || blocks=8
	printf '%s\n' "$RELEASE_KEYS" >"$WORK/$prefix.keys"
	while IFS= read -r key; do
		[ -n "$key" ] || continue
		printf 'echofog-release namespaces="%s" %s\n' "$ns" "$key" >"$WORK/$prefix.allowed"
		i=1
		while [ "$i" -le "$blocks" ]; do
			if ssh-keygen -Y verify -f "$WORK/$prefix.allowed" -I echofog-release -n "$ns" -s "$WORK/$prefix.sig.$i" <"$data" >/dev/null 2>&1; then
				count=$((count + 1))
				break
			fi
			i=$((i + 1))
		done
	done <"$WORK/$prefix.keys"
	echo "$count"
}

# read_record validates release.txt line by line (it is signed, but a shell parses it, so only exact
# shapes pass) and sets R_VERSION, R_MIN, R_EPOCH, R_AT, R_HASH and R_SIZE for this platform.
read_record() {
	R_VERSION=''
	R_MIN=''
	R_EPOCH=''
	R_AT=''
	R_HASH=''
	R_SIZE=''
	first=1
	while IFS= read -r line || [ -n "$line" ]; do
		if [ "$first" = 1 ]; then
			[ "$line" = "echofog-release 1" ] || die "release.txt: unknown format"
			first=0
			continue
		fi
		oldifs=$IFS
		IFS=' '
		# shellcheck disable=SC2086
		set -- $line
		IFS=$oldifs
		case "${1:-}" in
		version)
			if [ $# -ne 2 ] || [ -n "$R_VERSION" ] || ! semver_ok "$2"; then die "release.txt: bad version line"; fi
			R_VERSION=$2
			;;
		min_version)
			if [ $# -ne 2 ] || [ -n "$R_MIN" ] || ! semver_ok "$2"; then die "release.txt: bad min_version line"; fi
			R_MIN=$2
			;;
		key_epoch)
			[ $# -eq 2 ] && [ -z "$R_EPOCH" ] || die "release.txt: bad key_epoch line"
			case "$2" in '' | *[!0-9]* | 0?*) die "release.txt: bad key_epoch" ;; esac
			[ "${#2}" -le 9 ] || die "release.txt: bad key_epoch"
			R_EPOCH=$2
			;;
		released_at)
			[ $# -eq 2 ] && [ -z "$R_AT" ] || die "release.txt: bad released_at line"
			case "$2" in '' | *[!0-9]*) die "release.txt: bad released_at" ;; esac
			R_AT=$2
			;;
		artifact)
			[ $# -eq 4 ] || die "release.txt: bad artifact line"
			case "$2" in '' | *[!a-z0-9-]*) die "release.txt: bad platform" ;; esac
			case "$3" in '' | *[!0-9a-f]*) die "release.txt: bad hash" ;; esac
			[ "${#3}" -eq 64 ] || die "release.txt: bad hash"
			case "$4" in '' | *[!0-9]* | 0?*) die "release.txt: bad size" ;; esac
			[ "${#4}" -le 9 ] || die "release.txt: bad size"
			if [ "$2" = "linux-$ARCH" ]; then
				[ -z "$R_HASH" ] || die "release.txt: the platform is listed twice"
				R_HASH=$3
				R_SIZE=$4
			fi
			;;
		*) die "release.txt: unknown line" ;;
		esac
	done <"$WORK/release.txt"
	[ -n "$R_VERSION" ] && [ -n "$R_MIN" ] && [ -n "$R_EPOCH" ] && [ -n "$R_AT" ] || die "release.txt: a required line is missing"
	[ -n "$R_HASH" ] || die "release.txt: no file for linux-$ARCH"
}

verify_release() {
	fetch "$ECHOFOG_RELEASE_URL/release.txt" "$WORK/release.txt" || die "download of release.txt failed"
	fetch "$ECHOFOG_RELEASE_URL/release.txt.sigs" "$WORK/release.txt.sigs" || die "download of release.txt.sigs failed"
	good=$(signers "$NS_RELEASE" "$WORK/release.txt" "$WORK/release.txt.sigs" rel)
	[ "$good" -ge "$THRESHOLD" ] || die "the release signature is not valid: $good of the $THRESHOLD required release keys signed release.txt. Nothing was installed"
	read_record

	# A newer key set exists: this script is too old to trust anything under it. The announcement
	# is signed by the keys of THIS script; one signed by anybody else is ignored.
	if fetch "$ECHOFOG_RELEASE_URL/rotation.txt" "$WORK/rotation.txt" 2>/dev/null && fetch "$ECHOFOG_RELEASE_URL/rotation.txt.sigs" "$WORK/rotation.txt.sigs" 2>/dev/null; then
		rgood=$(signers "$NS_ROTATION" "$WORK/rotation.txt" "$WORK/rotation.txt.sigs" rot)
		if [ "$rgood" -ge "$THRESHOLD" ]; then
			new_epoch=$(sed -n 's/^new_epoch \([0-9][0-9]*\)$/\1/p' "$WORK/rotation.txt" | head -n 1)
			new_fp=$(sed -n 's/^new_keys_fingerprint \([0-9a-f]\{64\}\)$/\1/p' "$WORK/rotation.txt" | head -n 1)
			if [ -n "$new_epoch" ] && [ -n "$new_fp" ] && [ "${#new_epoch}" -le 9 ] && [ "$new_epoch" -gt "$KEY_EPOCH" ]; then
				die "the release keys were rotated (key epoch $new_epoch). This install.sh is out of date: download the current install.sh from the release pages and compare the SHA-256 of the whole file (sha256sum install.sh) with the published one; its key block must have the value $new_fp on the keys line of sh install.sh --fingerprint. Nothing was installed"
			fi
		fi
	fi

	[ "$R_EPOCH" = "$KEY_EPOCH" ] || die "the release is of key epoch $R_EPOCH, this script is of key epoch $KEY_EPOCH: get the install.sh that belongs to the release. Nothing was installed"

	v=$(semver_val "$R_VERSION")
	[ "$v" -ge "$(semver_val "$R_MIN")" ] || die "version $R_VERSION is below the min_version $R_MIN of the signed release; it is never installed"
	semver_ok "$MIN_VERSION_FLOOR" || die "this script has a bad version floor"
	[ "$v" -ge "$(semver_val "$MIN_VERSION_FLOOR")" ] || die "version $R_VERSION is below the floor $MIN_VERSION_FLOOR of this script; it is never installed"

	installed_bin="${ECHOFOG_INSTALLED_BIN:-/usr/local/bin/echofog-node}"
	if [ -x "$installed_bin" ]; then
		cur=$("$installed_bin" version 2>/dev/null | tr -d '\r' | tail -n 1 | tr ' ' '\n' | tail -n 1 || true)
		cur=${cur#v}
		if semver_ok "$cur"; then
			if [ "$v" -lt "$(semver_val "$cur")" ]; then
				if [ "$ALLOW_ROLLBACK" = 1 ]; then
					echo "install.sh: WARNING: this is a rollback from $cur to $R_VERSION. Older versions may carry fixed vulnerabilities." >&2
				else
					die "version $R_VERSION is older than the installed version $cur. A rollback needs --allow-rollback"
				fi
			fi
		else
			echo "install.sh: WARNING: the version of the installed binary could not be read; the rollback check is skipped" >&2
		fi
	fi

	fetch "$ECHOFOG_RELEASE_URL/manifest.bin" "$WORK/manifest.bin" 1048576 || die "download of manifest.bin failed"
	fetch "$ECHOFOG_RELEASE_URL/echofog-node-linux-$ARCH" "$BIN" 268435456 || die "download of the binary failed"
	size=$(wc -c <"$BIN" | tr -d ' ')
	sum=$(sha256sum "$BIN" | cut -d' ' -f1)
	if [ "$size" != "$R_SIZE" ] || [ "$sum" != "$R_HASH" ]; then
		rm -f "$BIN"
		die "the downloaded binary does not match the signed release record (size or SHA-256). It was deleted and nothing was installed"
	fi
}

verify_release
chmod 0700 "$BIN"

"$BIN" v2 install --binary "$BIN" --manifest "$WORK/manifest.bin" --platform "linux-$ARCH" "$@"
