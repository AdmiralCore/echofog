Русская версия: [deploy-vps-v2.ru.md](deploy-vps-v2.ru.md)

# A protocol v2 node on a VPS

This guide sets up an EchoFog v2 server node (relay, mailbox, optionally an exit) on a rented Linux
server. The older guide for protocol v1 is [deploy-vps.md](deploy-vps.md). All addresses below are
placeholders from the documentation ranges: `203.0.113.10` stands for the public IP address of your
server.

## 1. What is needed

- A Linux server (Ubuntu 22.04 or newer, Debian 12 or newer) with a public IPv4 or IPv6 address and
  root access.
- One UDP port reachable from the internet. 443 is the default and blends with HTTP/3 traffic.
- Optionally a domain whose DNS points at the server (for the node's own cover). Without a domain the
  node works for its own people only: it has no public card.
- The node serves **UDP only** (the noise3 carrier). TCP ports stay closed.

## 2. Install with Docker

The image is built from `deploy/v2/Dockerfile` and runs as an unprivileged user with a read-only file
system and no capabilities.

```
cp deploy/v2/.env.example deploy/v2/.env      # then edit it: set ECHOFOG_ADVERTISE_ADDR=203.0.113.10
docker compose -f deploy/v2/docker-compose.yml --env-file deploy/v2/.env up -d
```

`ECHOFOG_ADVERTISE_ADDR` is required on the first start and must be an IP literal. The container
refuses to start without it. `ECHOFOG_ROLES` chooses the roles; an empty value switches every role off.

## 3. Install without Docker

`deploy/v2/install.sh` downloads the binary and the signed release manifest over https and starts the
node's own installer. **The binary is checked before anything is installed**: the installer verifies it
against the manifest (two signatures of the three release keys of the build) and writes no file and runs
no system command until that passes. Download the script, read it, then run it as root. Never pipe a
download into a shell.

```
ECHOFOG_RELEASE_URL=https://example.org/releases/v2 sh install.sh --preset relay --advertise 203.0.113.10 --lang en
```

The installer creates the system user `echofog`, the data folder `/var/lib/echofog` (mode 0700), a
hardened systemd unit, and prints the commands that open the firewall. It does not change the
firewall and does not start the service unless `--start` is given. Without `--preset` it asks the
questions in the terminal (language, role, domain, port, address). Presets: `relay`, `exit-free`
(shares this server's connection: the responsibility for that traffic stays with the operator),
`home-entry`. The paid and private exits come later.

Then start it: `systemctl enable --now echofog-node`.

**What this check does not cover (the trust anchor).** The check is made by the downloaded binary
itself, with the release keys built into it, and the script runs that binary as root before any check
has been made. The signatures protect against a damaged file and against a forged manifest next to an
honest binary. They do **not** protect against someone who controls the download channel and serves a
hostile binary together with a matching manifest: such a binary would simply not check anything. The
trust anchor has to come from outside the download: compare the fingerprints of the release keys with
the ones published on the project's website through another channel, or verify a signature of
`install.sh` itself, before running it. If in doubt, build the binary from the source and pass it with
`--binary` and `--manifest` yourself.

## 4. First start and the recovery phrase

The installer prints the **recovery phrase once**. Write it down and keep it away from the server. With
Docker the phrase is in `recovery-phrase.txt` inside the data volume (it is never written to the
container log); copy it and delete the file:

```
docker compose -f deploy/v2/docker-compose.yml exec node cat /data/recovery-phrase.txt
docker compose -f deploy/v2/docker-compose.yml exec node rm /data/recovery-phrase.txt
```

The keys live in the data folder with mode 0600 and appear in no log.

## 5. Check that people can reach it

```
echofog-node v2 doctor --data /var/lib/echofog
echofog-node v2 status --data /var/lib/echofog
```

`doctor` checks the configuration, file permissions, the UDP port, connected peers, the clock, free
disk space, and the DNS name and certificate of the cover. It sends nothing out unless `--dns` is
given. Whether the port is reachable from outside is judged by a peer connecting: `status` shows the
number of sessions. Allow the UDP port in the firewall and in the host's panel:

```
ufw allow 443/udp
```

## 6. Presets and the firewall

| Preset | What it does |
|---|---|
| relay | relay, mailbox and DHT for others; no exit |
| exit-free | the same plus a free exit |
| home-entry | a relay with a separate invitation port, for wired networks (mobile allow-list networks need a verified address) |

An invitation port (`ECHOFOG_INVITES=true`, or the `home-entry` preset) is one more UDP port: open it
too. The operator API listens on `127.0.0.1` only and must never be published.

## 7. Updating

`echofog-node v2 update --manifest-url https://example.org/releases/v2/manifest.bin` looks for a newer
version, asks before installing it and never goes back to an older one. With Docker the image is
updated by changing the tag and restarting the container.

## 8. Troubleshooting

- `doctor` reports the port in use: another process holds the UDP port; stop it or choose another port.
- No peers connect: the firewall or the host's panel blocks the UDP port.
- The clock differs: enable time synchronisation (`systemd-timesyncd` or `chrony`).
- "No public card": the node has no cover; it serves its own people. A cover needs a domain and a
  camouflaged listener, which later releases add.
