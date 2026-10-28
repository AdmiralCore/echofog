Русская версия: [operator-api.ru.md](operator-api.ru.md)

# Operator API: your own payment and moderation

EchoFog has payment in TON built in, but you do not have to use it. If you run a node and want to
take money your own way (a Telegram bot, a website with card payments, cash from friends) or just
decide yourself who may use your node and for how long, use **passes**.

A pass is a personal invitation link to your node. Each pass has:

- its own link (`echofog://join?...`), which works in every EchoFog app like any invitation;
- a term: so many days, or no end;
- a number of devices that may use it (2 by default);
- optionally, an amount of traffic through your exit (so many GB).

You make a pass when somebody pays and send them the link. When they pay again, you extend it.
When the term ends or the traffic runs out, their devices are disconnected and cannot come back
until you extend the pass. You can revoke a pass at any moment, and you never have to change the
link of anybody else.

You can do all of it by hand with `echofog-node pass`, or from your own program over a small HTTP
API.

## What you need

- A node in **hidden** or **isolated** mode: `ECHOFOG_ACCESS=hidden` or `isolated` in `deploy/.env`.
  A public node lets everybody in, so passes mean nothing there. See
  [Hidden and isolated nodes](hidden-nodes.md).
- If you take payment your own way, set `ECHOFOG_EXIT_PRICE=0`. Otherwise the exit also asks pass
  holders to pay per GB in TON.
- For the API: an address and a token, see below.

The node's own invitation link (`echofog-node invite`) keeps working next to passes. People who
joined with it stay as they are; give it only to people you want to let in for good, or not at
all.

## By hand: `echofog-node pass`

On the server, in `deploy/`:

```bash
docker compose exec node echofog-node pass new --days 30 --devices 2 --label "Anna"
```

```
Pass 3f9a1c07be12 (active)  Anna
Term:     until 2026-11-28 14:05
Devices:  0 of 2
Traffic:  0 B used
Link (give it to that person only):
echofog://join?v=1&net=echofog&p=102&id=5f09...&s=...&a=203.0.113.24%3A443
```

| Command | What it does |
|---|---|
| `pass list` | every pass: state, term, devices, traffic, label |
| `pass new --days 30 --devices 2 --gb 100 --label "..."` | a new pass; `--days 0` never ends, `--gb 0` no traffic limit |
| `pass show ID` | one pass with its devices and link |
| `pass extend ID --days 30` | add 30 days (to the end, or to now if it ended) |
| `pass extend ID --gb 50` | add 50 GB |
| `pass extend ID --days 30 --reset` | another month, traffic counted from zero |
| `pass revoke ID` | stop it now and disconnect its devices (`extend` lets them back) |
| `pass delete ID` | forget it |

Every command is `docker compose exec node echofog-node pass ...`.

## From your program: the HTTP API

Turn it on in `deploy/.env`:

```bash
ECHOFOG_API_ADDR=0.0.0.0:8090
ECHOFOG_API_TOKEN=<at least 16 characters, e.g. from: openssl rand -hex 24>
```

and in `deploy/docker-compose.yml` uncomment the line under `ports`:

```yaml
      - "127.0.0.1:8090:8090"
```

Then `docker compose up -d`. The API is now at `http://127.0.0.1:8090` on the server itself. Keep
it there: a bot or a site on the same server reaches it, the Internet does not. If your bot runs
elsewhere, put it behind HTTPS (a reverse proxy such as Caddy) and never send the token over plain
HTTP.

Every request carries the token:

```
Authorization: Bearer <token>
```

### Requests

| Request | Body | Answer |
|---|---|---|
| `GET /api/v1/node` | | `{"mode", "link", "joined", "passes"}` |
| `GET /api/v1/passes` | | every pass, newest first |
| `POST /api/v1/passes` | `{"label", "days", "devices", "gb"}` (all optional) | the new pass, `201` |
| `GET /api/v1/passes/{id}` | | one pass |
| `POST /api/v1/passes/{id}/extend` | `{"days", "gb", "devices", "label", "resetUsage"}` | the pass |
| `POST /api/v1/passes/{id}/revoke` | | the pass, now `revoked` |
| `DELETE /api/v1/passes/{id}` | | `204` |
| `DELETE /api/v1/passes/{id}/devices/{peerId}` | | `204`: frees one device's place |

A pass looks like this:

```json
{
  "id": "3f9a1c07be12",
  "label": "tg:123456789",
  "link": "echofog://join?v=1&net=echofog&...",
  "state": "active",
  "created": 1793023500,
  "expires": 1795615500,
  "maxDevices": 2,
  "quotaBytes": 107374182400,
  "usedBytes": 5368709120,
  "devices": [
    {"peerId": "a1b2...", "joinedAt": 1793023800, "connected": true}
  ]
}
```

- `state` is `active`, `expired`, `used_up` (traffic spent) or `revoked`.
- `expires` is 0 for a pass with no end; times are Unix seconds.
- `quotaBytes` is 0 when there is no traffic limit; `usedBytes` counts both directions through
  your exit.
- `label` is yours: put a customer's Telegram ID or an order number there to find the pass later.
- When you make a pass, `days: 0` means no end, `devices: 0` means 2, `gb: 0` means no limit. When
  you extend, `days` and `gb` are added; `devices` and `label` change only when you send them.
  Extending also lifts a revocation.

Errors come as `{"error": "..."}`: `401` without the right token, `404` for an unknown pass, `409`
when the node is not hidden or isolated, `400` for a bad request.

### Try it with curl

```bash
T=<token>
curl -s -H "Authorization: Bearer $T" -d '{"label":"Anna","days":30,"gb":100}' http://127.0.0.1:8090/api/v1/passes
curl -s -H "Authorization: Bearer $T" http://127.0.0.1:8090/api/v1/passes
curl -s -H "Authorization: Bearer $T" -d '{"days":30}' http://127.0.0.1:8090/api/v1/passes/3f9a1c07be12/extend
curl -s -H "Authorization: Bearer $T" -X POST http://127.0.0.1:8090/api/v1/passes/3f9a1c07be12/revoke
```

## Ways to use it

### A Telegram bot that sells a monthly subscription

There is a ready one: [echofog-core-oss/deploy/tg-bot](../echofog-core-oss/deploy/tg-bot/README.md).
It hands out passes to the people you name and, if you set a price, sells them for Telegram Stars.
It starts with one more compose file next to the node. How it works inside:

1. A user writes `/buy` to your bot. The bot shows a payment button of whatever payment service
   you use.
2. The payment service tells the bot the payment went through.
3. The bot looks for a pass with `label` = the user's Telegram ID (`GET /api/v1/passes`). If there
   is none, it makes one: `POST /api/v1/passes {"label":"tg:123456789","days":30,"devices":2}`. If
   there is, it extends it: `POST /api/v1/passes/{id}/extend {"days":30}`.
4. The bot sends the user the `link`. The user pastes it into EchoFog (Proxy → Specific node).

When the month ends and the user does not pay, you do nothing: the pass expires and their devices
are disconnected. When they pay later, step 3 extends the same pass, and the same link works again.

A minimal sketch in Python (`requests`), the part that talks to the node:

```python
import requests

API = "http://127.0.0.1:8090/api/v1"
HEAD = {"Authorization": "Bearer " + TOKEN}

def paid(tg_id: int, days: int = 30) -> str:
    label = f"tg:{tg_id}"
    passes = requests.get(f"{API}/passes", headers=HEAD).json()
    mine = next((p for p in passes if p["label"] == label), None)
    if mine is None:
        mine = requests.post(f"{API}/passes", headers=HEAD,
                             json={"label": label, "days": days, "devices": 2}).json()
    else:
        mine = requests.post(f"{API}/passes/{mine['id']}/extend", headers=HEAD,
                             json={"days": days}).json()
    return mine["link"]  # send this to the user
```

### A website with a checkout

The same as the bot: the payment service calls your site's webhook, the site makes or extends a
pass with the order or customer number as the `label`, and shows the link on the "thank you" page
or sends it by e-mail. A customer page can show what is left from `GET /api/v1/passes/{id}`:
`expires`, `usedBytes` of `quotaBytes`, which devices are connected.

### Traffic packs instead of a term

Sell gigabytes: `{"gb": 50}` with no `days`. When the pack is used up the pass is `used_up` and its
devices are disconnected. The next purchase adds to it: `extend {"gb": 50}`. Or combine both: a
month with 200 GB, `extend {"days": 30, "gb": 200, "resetUsage": true}` every month.

### A trial

`{"days": 1, "devices": 1, "label": "trial:..."}`. If the person buys, extend the same pass; the
link they already set up keeps working.

### Family and friends, without money

One pass per person, with no end: `pass new --label "mum"`. When a phone is lost, take that one
device off (`DELETE .../devices/{peerId}`) and the person joins again from the new one. When you
no longer want to share with somebody, revoke their pass; nobody else has to change anything.
With the shared link you would have to make a new one and send it to everybody.

### Moderation

`GET /api/v1/passes` shows who uses how much and which devices are connected. A pass that eats your
monthly traffic: give it a limit (`extend {"gb": ...}` on a pass that had none sets one) or revoke
it. You can also cap free traffic for everybody with the exit settings in
[Run a node](run-a-node.md).

## What to keep in mind

- **The link is the key.** Whoever has the link of a pass can use it, up to its number of devices.
  Send it to that person only. If it leaks, revoke the pass and make a new one.
- **Each node has its own passes.** If you run several servers, make the pass on each one and send
  each link, or run them with one shared secret and the shared link
  ([Hidden and isolated nodes](hidden-nodes.md)).
- **Traffic is counted on your exit**, in both directions. It is saved every 30 seconds, so a crash
  loses at most that much of the count.
- **A node holds at most 10 000 passes.** Delete the old ones you no longer need.
- **The exit sees where people go**, as any VPN exit does (addresses and site names). Tell your
  customers that; the content is protected by the sites' own HTTPS.
- **The token is the whole API.** Whoever has it can make passes. Keep it out of the code you
  publish, and keep the API on loopback.
- EchoFog is experimental software. It has not been audited and has not been tested against a real
  national DPI. If you sell access, say so to the people who pay.
