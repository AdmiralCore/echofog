Русская версия: [deploy-vps-v2.ru.md](deploy-vps-v2.ru.md)

# A protocol v2 node on a VPS

This guide sets up an EchoFog v2 server node (relay, mailbox, optionally an exit) on a rented Linux
server. The older guide for protocol v1 is [deploy-vps.md](deploy-vps.md). All addresses below are
placeholders from the documentation ranges: `203.0.113.10` stands for the public IP address of your
server.

## 1. What is needed

- A Linux server (Ubuntu 22.04 or newer, Debian 12 or newer) with a public IPv4 or IPv6 address and
  root access.
- One public port reachable over **TCP and UDP**. 443 is the default and blends with ordinary HTTPS
  and HTTP/3 traffic: reality2-quic and reality2-tcp listen there, noise3 moves to 444.
- Optionally a domain whose DNS points at the server and a folder with the operator's own site (the
  own cover). With both, a stranger or a probe on 443 gets that real site. Without them the node
  works for its own people only: it has no public card.

## 2. Install with Docker

The image is built from `deploy/v2/Dockerfile` and runs as an unprivileged user with a read-only file
system and no capabilities.

```
cp deploy/v2/.env.example deploy/v2/.env      # then edit it: set ECHOFOG_ADVERTISE_ADDR=203.0.113.10
docker compose -f deploy/v2/docker-compose.yml --env-file deploy/v2/.env up -d
```

`ECHOFOG_ADVERTISE_ADDR` is required on the first start and must be an IP literal. The container
refuses to start without it. `ECHOFOG_ROLES` chooses the roles; an empty value switches every role off.

With a cover, set `ECHOFOG_COVER=node.example.org:443` and `ECHOFOG_SITE_DIR` to the folder with the
operator's site (index.html and the rest). Both are required together: a cover without the site is
refused, because a foreign site is not served. `ECHOFOG_COVER_CERT` optionally points at a PEM bundle
with the domain's chain and private key; empty means ACME. `ECHOFOG_ACME_ACCEPT_TOS` (the default,
`true`) records the operator's acceptance of the CA's terms; set it to `false` to refuse ACME and
require a certificate. `ECHOFOG_ACME_EMAIL` is an optional contact. Publish both 443/tcp and 443/udp,
and the noise3 port 444/udp, as `deploy/v2/docker-compose.yml` does.

## 3. Install without Docker

`deploy/v2/install.sh` downloads the binary over https and starts the node's own installer. **The trust
root is the release public key block written inside `install.sh`.** The script checks the signatures of
the signed release record (`release.txt`) with `ssh-keygen -Y verify` (two of the three release keys must
sign it) and compares the size and SHA-256 of the binary with that record. The downloaded file becomes
executable and is started only after that; if any check fails, it is deleted and nothing is installed.
The node's own installer (`echofog-node v2 setup`) then checks the binary once more against the signed
manifest. With a terminal, `setup` shows the full-screen installer (presets, system check, one summary
screen); over a pipe or `ssh -T` it falls back to the same questions one line at a time and never
prints the recovery phrase or the operator token — they are left in files under the data folder.

So the one thing to check by hand is that `install.sh` itself is the genuine file: the SHA-256 of the
**whole file** must be the value published for the release. Never pipe a download into a shell.
Download the file, compare the value, and only then run it as root:

```
curl --proto '=https' --tlsv1.2 -fsSL -o install.sh https://example.org/install.sh
sha256sum install.sh
```

The printed value must be exactly the one published on the project's website **and** in the README of
the repository (two independent places; if they differ, stop). `sha256sum` runs no code from the file.
The value changes with every release, because the version floor is part of the file: take the one
published for the release that is installed. The script's own `sh install.sh --fingerprint` is not a
check, since a changed script can print anything. Then:

```
ECHOFOG_RELEASE_URL=https://example.org/releases/v2 sh install.sh --preset relay --advertise 203.0.113.10 --lang en
```

The script needs `curl`, `sha256sum` and `ssh-keygen` (OpenSSH 8.0 or newer, package `openssh-client`);
without `ssh-keygen` it stops before running anything. It never installs a version below the signed
`min_version` of the release, below the floor stamped into the script, or below the version already
installed. A deliberate rollback above the first two bounds needs `--allow-rollback` and prints a
warning. When the release keys are replaced, an old `install.sh` stops and asks for the new one; the
procedure is in [release-trust.md](release-trust.md).

The installer creates the system user `echofog`, the data folder `/var/lib/echofog` (mode 0700), a
hardened systemd unit, and prints the commands that open the firewall. It does not change the
firewall and does not start the service unless `--start` is given. Without `--preset` it asks the
questions in the terminal (language, role, domain, site folder, port, address). With a domain the
installer serves reality2-quic and reality2-tcp on the chosen port and noise3 on the next one, and
writes the public card. Presets: `relay`, `exit-free` (shares this server's connection: the
responsibility for that traffic stays with the operator), `home-entry`. The paid and private exits
come later.

Then start it: `systemctl enable --now echofog-node`.

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

`doctor` checks the configuration, file permissions, the TCP and UDP ports, connected peers, the
clock, free disk space, the cover (its domain, site folder and that it is the operator's own) and
the certificate of the cover. It sends nothing out unless `--dns` is given. Whether the port is
reachable from outside is judged by a peer connecting: `status` shows the number of sessions and the
public link when the node has one. Allow both transports in the firewall and in the host's panel:

```
ufw allow 443/tcp
ufw allow 443/udp
ufw allow 444/udp
```

## 6. Presets and the firewall

| Preset | What it does |
|---|---|
| relay | relay, mailbox and DHT for others; no exit |
| exit-free | the same plus a free exit |
| home-entry | a relay with a separate invitation port, for wired networks (mobile allow-list networks need a verified address) |

An invitation port (`ECHOFOG_INVITES=true`, or the `home-entry` preset) is one more UDP port: open it
too (445 with a cover, 444 without). The operator API listens on `127.0.0.1` only and must never be
published.

## 7. Updating

`echofog-node v2 update --manifest-url https://example.org/releases/v2/manifest.bin` looks for a newer
version, asks before installing it and never goes back to an older one. With Docker the image is
updated by changing the tag and restarting the container.

## 8. Troubleshooting

- `doctor` reports the port in use: another process holds the UDP port; stop it or choose another port.
- No peers connect: the firewall or the host's panel blocks the UDP port.
- The clock differs: enable time synchronisation (`systemd-timesyncd` or `chrony`).
- "No public card": the node has no cover; it serves its own people over noise3. A public card needs
  both a cover domain and the operator's site folder; a cover without the site is refused.
