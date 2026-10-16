Русская версия: [deploy-vps.ru.md](deploy-vps.ru.md)

# Running a node on a VPS with Docker

Any VPS with a public IP and Ubuntu or Debian will do. This page is the short recipe; what the
roles and settings mean is in [Run a node](run-a-node.md).

## 1. Connect and install Docker

```bash
ssh root@<server IP>
```

```bash
curl -fsSL https://get.docker.com | sh
```

If you log in as a regular user instead of root, add yourself to the `docker` group and log in
again: `sudo usermod -aG docker "$USER"`.

## 2. Open the port

The node uses one port for UDP (QUIC) and TCP (the fallback for networks that block UDP). The
compose file publishes it as **443**. If the server runs `ufw`:

```bash
ufw allow 443/udp && ufw allow 443/tcp
```

If your provider has its own firewall in the control panel, open UDP and TCP 443 there too.

## 3. Get the code and configure

```bash
git clone https://github.com/AdmiralCore/EchoFog.git
cd EchoFog/echofog-core-oss
cp deploy/.env.example deploy/.env
nano deploy/.env
```

The settings that matter most:

```bash
# signaling only (DHT + help with hole punching, no traffic) — cheap; or false for a full relay
ECHOFOG_SIGNALING=false
ECHOFOG_RELAY=true
# share this server's connection as a paid exit (other people's traffic leaves from its IP)
ECHOFOG_EXIT=false
ECHOFOG_ADVERTISE_PORT=443
# empty for the very first node; otherwise the join address of a node already running
ECHOFOG_BOOTSTRAP=
```

## 4. Start

```bash
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs -f
```

On the first start the node prints a **recovery phrase** — write it down and keep it offline; it
restores the node's identity and wallet. It also prints a **Join address**:
`<this server's public IP or domain>:443/<peer id>`. Put the server's IP in front: that is what
other nodes use in `ECHOFOG_BOOTSTRAP` and what app users paste into **Settings → Bootstrap
nodes**.

Check that it is healthy:

```bash
docker compose -f deploy/docker-compose.yml ps
```

## TCP fallback

The node accepts TCP only while it imitates a real HTTPS site plausible for its IP. It looks for
one in its own subnet by itself; inside Docker it often cannot, so it runs on UDP only and logs an
`ANTI-DPI` warning. To turn TCP on, set a real TLS 1.3 site hosted in the same datacenter and its
IP in `deploy/.env`, then run `up -d` again:

```bash
ECHOFOG_TLS_SERVER_NAME=<site domain>
ECHOFOG_CAMOUFLAGE_ADDR=<site IP>:443
```

Details: [Anti-DPI](anti-dpi.md).

## Update

```bash
cd ~/EchoFog && git pull
cd echofog-core-oss && docker compose -f deploy/docker-compose.yml up -d --build
```

The identity, database and wallet live in the `echofog-data` volume and survive updates.
