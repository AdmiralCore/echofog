Русская версия: [README.ru.md](README.ru.md)

# Pass bot

A small Telegram bot that runs next to your EchoFog node and hands people **passes**: personal
links to the node with a term and a number of devices. What passes are is described in
[docs/operator-api.md](../../../docs/operator-api.md).

The bot does two things:

- **Hands them out to your people.** You (an admin) write `/pass anna`, the bot answers with a link,
  you forward it to Anna. With no end, or for as long as you say: `/pass anna 30`.
- **Sells them for Telegram Stars**, if you set a price. Somebody writes `/buy`, pays in Stars inside
  Telegram, and the bot sends them the link at once. When they pay again, the same pass is extended
  and the link stays the same. Stars need no card, no payment provider and no company: they work in
  any bot with no setup.

The bot reaches the node's API inside the Docker network; no port is opened to the outside.

## Running it

You need a node set up with the [VPS guide](../../../docs/deploy-vps.md), in `isolated` or `hidden`
mode.

1. **The bot.** In Telegram open @BotFather, send `/newbot` and pick a name. It sends a token like
   `123456:ABC...`.
2. **Your ID.** Write to @userinfobot; it answers with your numeric ID. That is the bot's admin.
3. **Settings.** Add to `deploy/.env`:

   ```bash
   ECHOFOG_ACCESS=isolated
   ECHOFOG_EXIT=true
   ECHOFOG_EXIT_PRICE=0            # people pay for the pass, not per GB
   ECHOFOG_API_ADDR=0.0.0.0:8090   # the API is seen only inside the Docker network
   ECHOFOG_API_TOKEN=<openssl rand -hex 24>

   TG_BOT_TOKEN=<token from BotFather>
   TG_ADMIN_IDS=<your ID>          # several, comma-separated
   TG_STARS_PRICE=0                # 0: only hand out; e.g. 150: sell
   TG_DAYS=30                      # the term one purchase buys
   TG_DEVICES=2                    # devices per pass
   ```

4. **Start it** (in `deploy/`):

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.bot.yml up -d --build
   docker compose -f docker-compose.yml -f docker-compose.bot.yml logs -f bot
   ```

5. Write `/start` to your bot.

## Commands

| Command | Who | What it does |
|---|---|---|
| `/pass NAME [DAYS]` | admin | a pass for a friend; again: extends it |
| `/list` | admin | every pass and its state |
| `/revoke ID` | admin | revoke a pass and drop its devices |
| `/refund USER CHARGE` | admin | give the Stars back and revoke the pass (the bot sends you that line with every payment) |
| `/buy` | anyone | buy `TG_DAYS` days for `TG_STARS_PRICE` Stars |
| `/my` | anyone | their own pass and link |
| `/paysupport` | anyone | a question about a payment |

The bot answers in Russian to people whose Telegram is set to Russian (or Ukrainian, Belarusian,
Kazakh) and in English to everybody else. `TG_LANG=en` or `TG_LANG=ru` in `.env` fixes one language
for everybody. The texts are in `TEXT` at the top of `bot.py`; change them or add a language, then
run `up -d --build` again.

## Worth knowing

- **The link is the key.** The bot sends it only to the buyer, or to you. Extending a pass keeps
  its link, so if a link leaks, revoke it (`/revoke ID`), delete it on the server
  (`docker compose exec node echofog-node pass delete ID`), and then `/pass` or a new purchase makes
  a pass with a new link.
- **A revocation alone does not last.** The next `/pass` for that name, or the next purchase by that
  person, extends the same pass, and extending lifts the revocation. To keep somebody off, delete
  the pass on the server as well.
- **Stars** collect on the bot's balance. You can withdraw them to TON through Fragment, under
  Telegram's rules and with the delay Telegram sets.
- **Refunds.** Telegram expects a paid bot to answer `/paysupport`, and can refund Stars. Answer
  people, and in a dispute use `/refund`.
- **You are responsible.** What you sell and to whom is the node owner's business. Telegram, your
  hoster and the laws of your country have rules; read them before you set a price.
- **The exit sees where people go,** like any VPN exit: addresses and site names. Tell your buyers.
- EchoFog is experimental: it has not been audited or tested against a real national DPI. Tell
  that to the people who pay, too.
