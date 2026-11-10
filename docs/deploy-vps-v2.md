Русская версия: [deploy-vps-v2.ru.md](deploy-vps-v2.ru.md)

# A protocol v2 node on a VPS

This guide takes an empty server to a working EchoFog v2 node: a relay, a mailbox and a DHT server,
optionally an exit. Every path, variable name and port here was checked against the files of the `v2`
branch: `deploy/v2/.env.example`, `deploy/v2/docker-compose.yml`, `deploy/v2/entrypoint.sh`,
`deploy/v2/Dockerfile` and `deploy/v2/install.sh`. All addresses are placeholders from the
documentation ranges: `203.0.113.10` stands for the public address of your server.

The guide for protocol v1 is [deploy-vps.md](deploy-vps.md).

> **Status.** Protocol v2 is the 0.3.0 line and still experimental: the code has had no independent
> security audit yet. Do not put more TON into a node's wallet than you are ready to lose.

## 1. What you get and what you need

The node takes the roles you give it: it relays calls and messages between people who cannot connect
directly, keeps messages for people who are offline, and holds a part of the DHT (the network's
shared address book). Roles are listed in `ECHOFOG_ROLES`; see [Run a node](run-a-node.md) for what
each one costs in traffic.

The container runs as an unprivileged user with a read-only root file system, no Linux capabilities
and no way to gain privileges. The operator API stays on loopback inside the container and is reached
through `docker compose exec`; no operator or debug port is published. The profile (identity,
`server.json`, operator token) lives in the `echofog-v2-data` volume at `/data`.

Without a cover site the node has **no public card**: it serves the people who already have its link
over noise3 and nothing else. It then listens on UDP only, so a TCP check on 443 is refused even
though the node is healthy. A public card needs both a domain that points at the server and a folder
with the operator's own site.

What the server needs:

- Ubuntu 22.04 or newer, or Debian 12 or newer, with root access.
- A public IPv4 or IPv6 address that the Internet can reach on the published ports. On some clouds
  the machine's own interface has a private address and the public one is NATed in front of it; that
  works as long as the provider forwards the ports.
- Port 443 reachable over **TCP and UDP**. The compose file caps the container at 1 GB of memory and
  512 processes, so plan at least that much free memory.
- Free disk for Docker and the image. This guide gives no fixed figure because the image changes with
  every build; check `df -h /` before starting. The node's own data is small, and the compose file
  caps container logs at 100 MB (five files of 20 MB).

## 2. Prepare a clean server

The commands below assume a root shell (`sudo -i`); prefix `sudo` if you work as an ordinary user.
Start with the system packages:

```sh
apt update && apt -y upgrade
apt -y install ca-certificates curl
```

Check the clock. A peer rejects a handshake when the two clocks differ by more than ten minutes, and
the cover certificate is checked against the system time:

```sh
timedatectl status        # "System clock synchronized: yes"
```

If it is not synchronised, install and enable a time service:

```sh
apt -y install systemd-timesyncd && timedatectl set-ntp true
```

Check the disk:

```sh
df -h /
```

## 3. Install git and Docker

The node is run from a git clone and from the published container image, so both are needed. Docker
is installed from Docker's own repository, which ships the `compose` plugin; the distribution
packages are older and may not have it. The commands below work on Ubuntu and Debian: `ID` in
`/etc/os-release` is `ubuntu` or `debian`, matching the repository paths.

```sh
apt -y install git
install -m 0755 -d /etc/apt/keyrings
. /etc/os-release
curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt update
apt -y install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
```

Check that the daemon and the compose plugin work:

```sh
docker version
docker compose version
docker run --rm hello-world
```

`hello-world` prints a greeting and removes itself. If it fails, fix Docker before going further;
nothing below will work.

An alternative is Docker's convenience script from `https://get.docker.com`. It installs the latest
release without pinning and without a signed apt source of your own, so read it before running it if
you use it.

The commands in this guide are for a root shell. If you work as an ordinary user, either prefix
`sudo` or add the user to the `docker` group and log in again:

```sh
usermod -aG docker youruser
```

Membership in the `docker` group is equivalent to root on this host: it allows starting a container
that mounts any host path. Keep that in mind on a shared machine.

## 4. Clone the `v2` branch

The repository holds both products and both protocols. The v2 node is on the `v2` branch; `main` and
`dev` still build the v1 node (`echofog-node run`). Clone only the branch you need:

```sh
git clone --branch v2 --single-branch https://github.com/AdmiralCore/echofog.git
cd echofog/echofog-core-oss
```

Check that you are on the right branch:

```sh
git branch --show-current     # v2
git log -1 --oneline
```

The container image is built by the `node-image` workflow from this branch and published as
`ghcr.io/admiralcore/echofog-node`. The tag `v2-dev` follows every push to `v2`; the tags `latest`
and `dev` are the v1 node and are not used here. Every build also gets `sha-<commit>`, which is what
you pin when you want updates only on purpose.

## 5. Configure `deploy/v2/.env`

```sh
cp deploy/v2/.env.example deploy/v2/.env
```

Pick the Compose command for your setup; the guide uses `$DC` for it from here on. Without a cover it
is the base file; with a cover add the override described below:

```sh
# no cover
DC="docker compose -f deploy/v2/docker-compose.yml --env-file deploy/v2/.env"
# or, with a cover and a mounted site
DC="docker compose -f deploy/v2/docker-compose.yml -f deploy/v2/docker-compose.site.yml --env-file deploy/v2/.env"
```

Edit `deploy/v2/.env`. The values that matter:

| Variable | What to put |
|---|---|
| `ECHOFOG_IMAGE` | `ghcr.io/admiralcore/echofog-node:v2-dev`, or `...:sha-<commit>` to pin a build. |
| `ECHOFOG_ADVERTISE_ADDR` | **Required on the first start.** The public IPv4 or IPv6 address of this server as an IP literal. A name is refused, and the container exits with an error. |
| `ECHOFOG_PORT` | `443`. With a cover, reality2-quic listens here over UDP and reality2-tcp over TCP; without a cover, noise3 listens here over UDP. |
| `ECHOFOG_NOISE_PORT` | `444`. Where noise3 moves when a cover takes `ECHOFOG_PORT`. |
| `ECHOFOG_ROLES` | `relay,mailbox,dht-server`. Roles are comma-separated; an empty value switches every role off. |
| `ECHOFOG_INVITES` | `true` to open one more UDP listener for invitation links. Without a cover it takes `ECHOFOG_PORT+1` (444), with a cover `ECHOFOG_NOISE_PORT+1` (445). |
| `ECHOFOG_COVER` | `node.example.org:443`. The name must resolve to `ECHOFOG_ADVERTISE_ADDR`. |
| `ECHOFOG_SITE_DIR` | `/srv/site`: the folder with the operator's own site. Required together with `ECHOFOG_COVER`; the container refuses a cover without it. |
| `ECHOFOG_COVER_CERT` | Empty means ACME. Otherwise the path of a PEM bundle with the domain's chain and private key, inside the container. |
| `ECHOFOG_ACME_ACCEPT_TOS` | `true` records your acceptance of the CA's terms. `false` refuses ACME and requires a certificate. |
| `ECHOFOG_ACME_EMAIL` | Optional contact for ACME. |
| `ECHOFOG_ACME_CHALLENGE` | Empty means `tls-alpn-01`, which needs TCP 443. `http-01` also needs TCP 80. The CA validates `tls-alpn-01` on port 443 of the domain, so a cover moved to another port needs `http-01` or a certificate. |
| `ECHOFOG_DATA_DIR` | Leave `/data`. The profile lives in the volume mounted there. |

**The exit role.** `exit` in `ECHOFOG_ROLES` shares this server's connection: other people's traffic
leaves the Internet from this IP address and the legal responsibility for it is the operator's. Turn
it on only on a server and in a country where you may do that, and read your hoster's terms first.
Details: [Run a node](run-a-node.md#about-running-an-exit).

**Roles, cover and invitations are applied once, when the profile is created.** After that the node
reads `/data/server.json`, and changing `.env` no longer affects them (the image tag and the published
ports do still come from `.env`). Plan these values before the first start. To change them later,
stop the container and edit `server.json` (it carries comments) in a temporary container that mounts
the same volume:

```sh
$DC stop node
docker run --rm -it -v v2_echofog-v2-data:/data alpine sh -c \
  'vi /data/server.json && chown 1000:1000 /data/server.json && chmod 600 /data/server.json'
$DC start node
```

Replace the volume name with the one `docker volume ls` shows. Do not create a new profile just to
change a role: a new `init` replaces the identity.

To add invitations to an existing profile, the key `server.json` needs is:

```json
"invite_listen": { "suite": "noise3", "bind": "0.0.0.0:445", "advertise": "203.0.113.10" }
```

Then publish the same port in the override (see below).

### The cover site and Docker

`deploy/v2/docker-compose.yml` mounts nothing but the data volume, so a host folder is not visible
inside the container. The node needs `ECHOFOG_SITE_DIR` to be a path inside the container, and that
folder has to be mounted. Add an override file:

```yaml
# deploy/v2/docker-compose.site.yml
services:
  node:
    volumes:
      - echofog-v2-data:/data
      - /srv/echofog-site:/srv/site:ro
    ports:
      - "445:445/udp"   # only with ECHOFOG_INVITES=true and a cover
```

Both mounts are listed on purpose: the container needs the profile volume and the site. The site is
mounted read-only, so the container's read-only file system stays as it is. Put the site in
`/srv/echofog-site` on the host and keep it world-readable: the container's user must be able to read
it, and a public site is not a secret. Then set `ECHOFOG_SITE_DIR=/srv/site`. If you use your own
certificate, mount it the same way and point `ECHOFOG_COVER_CERT` at the path inside the container.

The invitation port deserves attention: the base compose file publishes `ECHOFOG_PORT` (TCP and UDP)
and `ECHOFOG_NOISE_PORT` (UDP) only, so with a cover and invitations the listener on 445 is not
reachable until the override publishes it. Without a cover, invitations use 444, which the base file
already publishes.

## 6. Firewall and the provider's panel

Allow SSH first, or you will lock yourself out. Check the SSH port your provider uses before running
these commands.

```sh
apt -y install ufw
ufw allow 22/tcp
ufw allow 443/tcp
ufw allow 443/udp
ufw allow 444/udp
ufw allow 445/udp     # only with invitations and a cover
ufw enable
ufw status
```

If the server is at a cloud provider, the same ports must be open in its own firewall or security
group; `ufw` alone is not enough. UDP matters as much as TCP: the main path is QUIC over UDP, and a
node whose UDP port is closed looks alive but gets no peers. A blocked UDP port cannot be tested with
a simple connection check, because nothing answers either way.

## 7. Start the node

`$DC` is the shorthand chosen in section 5. Check what Compose is about to do, then pull and start:

```sh
$DC config
$DC pull
$DC up -d
```

`config` prints the resolved configuration, including the environment from `.env`: check the image
and the published ports before the first start, and do not paste the output into a public issue. The
image is published for `linux/amd64` and `linux/arm64`; on another architecture build it
from `echofog-core-oss` with `docker build -f deploy/v2/Dockerfile -t echofog-node:v2-local .` and set
`ECHOFOG_IMAGE=echofog-node:v2-local`.

On the first start the entrypoint creates the profile in the volume and then runs the node. Watch it:

```sh
$DC ps
$DC logs -f --tail=100 node
```

`ps` shows the container and its health. The image has a healthcheck that runs
`echofog-node v2 status --data /data` every 30 seconds, with a 40-second start period and three
retries: `healthy` means the node is up and its operator API answers on loopback, `unhealthy` means
the process is running but the API does not answer, and `starting` is normal for the first seconds.
`logs` is where the reason for a restart loop appears; the most common one is a missing or malformed
`ECHOFOG_ADVERTISE_ADDR`.

If `pull` is refused with a permission or "denied" error, the package may need an authenticated
request. Create a GitHub personal access token with the `read:packages` scope and log in; the command
asks for the token, so it does not land in the shell history:

```sh
docker login ghcr.io -u <github-user>
```

Do not put the token into `.env`, a script or the repository. A "manifest unknown" error instead
means the tag does not exist; check `deploy/v2/.env` and the image's tags.

## 8. First start: recovery phrase, status, invitations, card

The recovery phrase is written once to a file in the volume and never to the container log. Copy it
somewhere safe, then delete the file:

```sh
$DC exec node cat /data/recovery-phrase.txt
$DC exec node rm /data/recovery-phrase.txt
```

The phrase restores the node's identity and its wallet. Anyone who has it controls the node; keep it
off the server.

Look at the node:

```sh
$DC exec node echofog-node v2 status --data /data --plain
$DC exec node echofog-node v2 doctor --data /data
```

`status` shows the roles, the carriers, sessions, traffic, the cover and certificate, and the public
link when the node has a public card. `--plain` drops colour, which keeps the output readable in a
log. `doctor` checks the configuration, file permissions, the ports, the clock, free disk space, the
cover (domain, site folder, that the site is the operator's own) and its certificate; it sends nothing
out unless you add `--dns`. When the node is running, it reports the ports as held by the node rather
than trying to bind them.

Invitations need `ECHOFOG_INVITES=true` **at the first start**; with the default `false` the node
answers that invitations are not configured. Create one with an access term:

```sh
$DC exec node echofog-node v2 invite create --data /data --access 30d
$DC exec node echofog-node v2 invite list --data /data
```

`invite create` prints the link; `--access 30d` is how long the person may use the node, `--link 7d`
would limit the link itself, and `--max-entries` limits how many people may use it. Hand the link over
a channel you trust.

**The public card.** With a cover, `status` prints the public link (and the matching entry for a node
list) once the card is issued. That needs the domain's DNS to point at `ECHOFOG_ADVERTISE_ADDR`, the
site folder to have content, and the certificate to be issued. Without a cover there is no public
card by design: the node works for its own people over noise3 only.

The installer's terminal screen draws a QR code for the link when a signed release makes it available
(section 12). The Docker path has no QR command: take the link from `status` or `invite create` and
encode it with any offline QR tool.

Check that the port is open from outside, from another machine:

```sh
nc -vz 203.0.113.10 443
```

Install `netcat-openbsd` there if `nc` is missing. That proves TCP only. For UDP the honest check is a
peer connecting: `status` shows sessions, and `doctor` reports reachability from the node's own point
of view. If nothing connects, the firewall, the provider's panel or a wrong `ECHOFOG_ADVERTISE_ADDR`
is the cause.

## 9. Updating

```sh
git pull --ff-only
$DC pull
$DC up -d
```

`git pull` updates the compose file, the example configuration and this guide; `pull` fetches the
image the tag points at; `up -d` recreates the container if anything changed. The profile stays in the
volume, so the identity, the configuration and the wallet survive.

To pin a build, set the image to its commit tag, for example
`ECHOFOG_IMAGE=ghcr.io/admiralcore/echofog-node:sha-1234567`, and run `$DC pull && $DC up -d`. An
update is then a deliberate change of that line. To roll back, put the previous `sha-` tag back and
run the same two commands: the data in the volume is not touched. If `git pull` changed
`docker-compose.yml` or the entrypoint, read the diff before starting the new image.

Back up the volume before a version change you are unsure about. `docker volume ls` shows the volume
name (Compose prefixes it with the project name, `v2_echofog-v2-data` by default); this copies the
profile out into the current folder, using the `tar` that the `alpine` image ships:

```sh
docker run --rm -v v2_echofog-v2-data:/data -v "$PWD":/backup alpine tar czf /backup/echofog-data.tar.gz -C /data .
```

## 10. Removing the node

```sh
$DC down
```

That removes the container and the network. The profile stays in the volume. To delete the data as
well:

```sh
$DC down -v
```

**This deletes the identity and the wallet.** Without the recovery phrase the node cannot be restored,
and any TON on its wallet becomes unreachable. Do this only when you mean it. Then remove the clone:

```sh
cd ../.. && rm -rf echofog
```

## 11. When something does not work

| Symptom | Cause | What to do |
|---|---|---|
| `up` fails with "address already in use" | Another process holds TCP or UDP 443: a web server, a proxy, an older node | `ss -lntup \| grep ':443'`, stop the other process, or set another `ECHOFOG_PORT` and open it in the firewall |
| Container restarts in a loop, log says `ECHOFOG_ADVERTISE_ADDR is required` | The variable is empty | Set the public IP literal in `.env` and `$DC up -d` |
| Log says `must be an IPv4 or IPv6 literal, not a name` | A hostname was put into `ECHOFOG_ADVERTISE_ADDR` | Use the address; the domain belongs in `ECHOFOG_COVER` |
| Log says `ECHOFOG_SITE_DIR is required with ECHOFOG_COVER` | A cover without a site folder | Set `ECHOFOG_SITE_DIR` and mount the folder (section 5), or clear `ECHOFOG_COVER` |
| `pull` is denied | The registry wants an authenticated request, or the tag is wrong | `docker login ghcr.io` with a `read:packages` token, or check the tag |
| No public card, while a cover is set | DNS does not point at the server, the site folder is empty or foreign, or the ACME order failed | `$DC exec node echofog-node v2 doctor --data /data --dns`, open TCP 443 (and 80 for `http-01`), check the site has `index.html` |
| No public card, no cover | That is the design | Add a domain and a site folder, or keep the node for its own people |
| Peers do not connect, `status` shows no sessions | UDP 443 is closed in `ufw` or in the provider's panel, or `ECHOFOG_ADVERTISE_ADDR` is not the address the Internet sees | Open 443/udp in both places; compare the address with the provider's console |
| Invitations are refused | `ECHOFOG_INVITES` was `false` at the first start | Edit `server.json` and add the invite listener (section 5), or, on a node not yet in use, start over with an empty volume; then publish the port |
| Clock warnings in `status` or `doctor` | Time is not synchronised | `timedatectl set-ntp true` |
| `doctor` reports little disk space | Docker images, logs or the host's own data | `docker system prune`, check `df -h /`; log size is capped by the compose file |
| `exec` says the node is not running | The container is stopped, or the profile has no `operator.addr` | `$DC ps`, `$DC logs --tail=100 node` |
| Health is `unhealthy` | The process is up but the operator API does not answer | Read the log; the API writes `operator.addr` in `/data` when it starts |

## 12. Installing without Docker

There is a native path for a server without Docker: `deploy/v2/install.sh` verifies a signed release
and then runs `echofog-node v2 setup`, which creates the service user `echofog`, the data folder
`/var/lib/echofog` (mode 0700) and a systemd unit, and enables the service. It does not change the
firewall by itself: it prints the commands and runs them only with `--open-ports`.

**It is not usable yet, and this guide does not pretend otherwise.** The `install.sh` in the repository
is a template: its release key block is `PLACEHOLDER`, so the script stops instead of verifying
anything. The open-source build of `echofog-node` also carries no release keys, so `v2 setup` stops
with "this build has no release keys". Both become usable when a signed release exists with the key
block in place; the trust root, the `min_version` rule and key rotation are described in
[Release trust](release-trust.md).

When that release exists, the sequence will be: download `install.sh` from the release page, compare
`sha256sum install.sh` with the value published on the site and in the README of the repository, and
only then run it as root with `ECHOFOG_RELEASE_URL` pointing at the release folder. The script checks
the signatures of `release.txt` (at least two of the three release keys), compares the size and
SHA-256 of the binary with that record, and starts the setup only after that. Never pipe a download
into a shell. Until then, Docker is the working path, and this is also why section 7 is the one to
follow.
