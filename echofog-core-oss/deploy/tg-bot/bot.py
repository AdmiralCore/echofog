"""A Telegram bot that hands out passes of a hidden EchoFog node, and can sell them for Stars.

/pass NAME [DAYS]   (admins) a pass for a friend, no end unless DAYS; again: adds DAYS
/revoke ID          (admins) stop a pass and drop its devices
/list               (admins) every pass
/refund USER CHARGE (admins) give the Stars back and revoke that user's pass
/buy                TG_DAYS days for TG_STARS_PRICE Stars (off while the price is 0)
/my                 your own pass and its link
It answers in Russian to people whose Telegram is in Russian (or Ukrainian, Belarusian, Kazakh)
and in English to everybody else; TG_LANG=ru or en fixes one. The texts are in TEXT below.
See README.md next to this file.
"""
import asyncio
import html
import os
from datetime import datetime

import aiohttp
from aiogram import Bot, Dispatcher, F
from aiogram.filters import Command, CommandObject
from aiogram.types import LabeledPrice, Message, PreCheckoutQuery

API = os.environ.get("ECHOFOG_API_URL", "http://node:8090/api/v1")
HEAD = {"Authorization": "Bearer " + os.environ["ECHOFOG_API_TOKEN"]}
ADMINS = {int(x) for x in os.environ.get("TG_ADMIN_IDS", "").replace(" ", "").split(",") if x}
PRICE = int(os.environ.get("TG_STARS_PRICE", "0"))
DAYS = float(os.environ.get("TG_DAYS", "30"))
DEVICES = int(os.environ.get("TG_DEVICES", "2"))
LANG = os.environ.get("TG_LANG", "")

TEXT = {
    "ru": {
        "my": "/my — ваш пропуск и ссылка", "buy": "/buy — {days:g} дней VPN за {price} ⭐",
        "support": "/paysupport — вопрос по оплате",
        "admin": "Админ: /pass ИМЯ [ДНЕЙ], /revoke ID, /list, /refund USER CHARGE",
        "active": "действует", "expired": "срок вышел", "used_up": "трафик кончился", "revoked": "отозван",
        "until": "до", "no_end": "бессрочно", "devices": "устройств {n} из {max}",
        "how": "Откройте ссылку: страница предложит открыть её в EchoFog или скачать приложение. Или в EchoFog: Прокси → Конкретная нода → вставьте ссылку из блока ниже.", "open": "Открыть пропуск",
        "pass_usage": "/pass ИМЯ [ДНЕЙ]", "none": "Пропусков пока нет.", "no_pass": "У вас пока нет пропуска.",
        "not_sold": "Здесь пропуска не продаются.", "invoice": "{days:g} дней, устройств: {devices}",
        "thanks": "Спасибо! ", "refund_usage": "/refund USER CHARGE (их присылает бот при каждой оплате)",
        "refunded": "Звёзды возвращены, пропуск отозван.",
        "paysupport": "Напишите владельцу бота: оплату можно вернуть.", "failed": "Не получилось: {e}",
    },
    "en": {
        "my": "/my — your pass and its link", "buy": "/buy — {days:g} days of VPN for {price} ⭐",
        "support": "/paysupport — a question about a payment",
        "admin": "Admin: /pass NAME [DAYS], /revoke ID, /list, /refund USER CHARGE",
        "active": "active", "expired": "expired", "used_up": "traffic used up", "revoked": "revoked",
        "until": "until", "no_end": "no end", "devices": "devices {n} of {max}",
        "how": "Open the link: the page offers to open it in EchoFog or to download the app. Or in EchoFog: Proxy → Specific node → paste the link from the block below.", "open": "Open the pass",
        "pass_usage": "/pass NAME [DAYS]", "none": "No passes yet.", "no_pass": "You have no pass yet.",
        "not_sold": "Passes are not sold here.", "invoice": "{days:g} days, {devices} devices",
        "thanks": "Thank you! ", "refund_usage": "/refund USER CHARGE (the bot sends them with every payment)",
        "refunded": "Stars refunded, pass revoked.",
        "paysupport": "Write to the owner of this bot: a payment can be refunded.", "failed": "That did not work: {e}",
    },
}
dp = Dispatcher()


def t(user, key, **kw):
    """The text key in the language of user's Telegram."""
    lang = LANG if LANG in TEXT else "en"
    if LANG not in TEXT and (getattr(user, "language_code", None) or "").lower().startswith(("ru", "uk", "be", "kk")):
        lang = "ru"
    return TEXT[lang][key].format(**kw)


async def call(method, path, body=None):
    async with aiohttp.ClientSession(headers=HEAD) as s, s.request(method, API + path, json=body) as r:
        data = {} if r.status == 204 else await r.json(content_type=None)
        if r.status >= 300:
            raise RuntimeError(data.get("error", r.status))
        return data


async def pass_of(label):
    return next((p for p in await call("GET", "/passes") if p["label"] == label), None)


async def grant(label, days):
    """Make the pass of this label, or extend the one it has."""
    old = await pass_of(label)
    if old is None:
        return await call("POST", "/passes", {"label": label, "days": days, "devices": DEVICES})
    return await call("POST", f"/passes/{old['id']}/extend", {"days": days})


def card(user, p):
    end = p.get("expires")
    term = t(user, "until") + " " + datetime.fromtimestamp(end).strftime("%d.%m.%Y") if end else t(user, "no_end")
    devices = t(user, "devices", n=len(p["devices"]), max=p["maxDevices"])
    web = p.get("webLink")  # an older node gives only the echofog:// link
    opener = f"<a href=\"{html.escape(web)}\">{t(user, 'open')}</a>\n\n" if web else ""
    return (f"{p['label']}: {t(user, p['state'])}, {term}, {devices}\nID {p['id']}\n\n"
            f"{opener}<code>{html.escape(p['link'])}</code>\n\n{t(user, 'how')}")


def admin(m: Message):
    return m.from_user.id in ADMINS


@dp.message(Command("start", "help"))
async def start(m: Message):
    u = m.from_user
    lines = ([t(u, "buy", days=DAYS, price=PRICE)] if PRICE else []) + [t(u, "my")]
    lines += [t(u, "support")] if PRICE else []
    lines += ["", t(u, "admin")] if admin(m) else []
    await m.answer("\n".join(lines))


@dp.message(Command("pass"), admin)
async def give(m: Message, command: CommandObject):
    args = (command.args or "").split()
    if not args:
        return await m.answer(t(m.from_user, "pass_usage"))
    days = float(args[1]) if len(args) > 1 else 0
    await m.answer(card(m.from_user, await grant("friend:" + args[0], days)), parse_mode="HTML")


@dp.message(Command("revoke"), admin)
async def revoke(m: Message, command: CommandObject):
    await m.answer(card(m.from_user, await call("POST", f"/passes/{command.args}/revoke")), parse_mode="HTML")


@dp.message(Command("list"), admin)
async def listing(m: Message):
    lines = [f"{p['id']}  {t(m.from_user, p['state'])}  {p['label']}" for p in await call("GET", "/passes")]
    await m.answer("\n".join(lines) or t(m.from_user, "none"))


@dp.message(Command("my"))
async def my(m: Message):
    p = await pass_of(f"tg:{m.from_user.id}")
    text = card(m.from_user, p) if p else t(m.from_user, "no_pass") + (" /buy" if PRICE else "")
    await m.answer(text, parse_mode="HTML")


@dp.message(Command("buy"))
async def buy(m: Message, bot: Bot):
    if not PRICE:
        return await m.answer(t(m.from_user, "not_sold"))
    text = t(m.from_user, "invoice", days=DAYS, devices=DEVICES)
    await bot.send_invoice(m.chat.id, title="EchoFog VPN", description=text, payload="pass",
                           currency="XTR", prices=[LabeledPrice(label="VPN", amount=PRICE)])


@dp.pre_checkout_query()
async def checkout(q: PreCheckoutQuery):
    await q.answer(ok=True)


@dp.message(F.successful_payment)
async def paid(m: Message, bot: Bot):
    p = await grant(f"tg:{m.from_user.id}", DAYS)
    await m.answer(t(m.from_user, "thanks") + card(m.from_user, p), parse_mode="HTML")
    for a in ADMINS:  # what /refund needs, if it comes to that
        await bot.send_message(a, f"⭐ {PRICE}: /refund {m.from_user.id} {m.successful_payment.telegram_payment_charge_id}")


@dp.message(Command("refund"), admin)
async def refund(m: Message, command: CommandObject, bot: Bot):
    args = (command.args or "").split()
    if len(args) != 2:
        return await m.answer(t(m.from_user, "refund_usage"))
    await bot.refund_star_payment(int(args[0]), args[1])
    if p := await pass_of(f"tg:{args[0]}"):
        await call("POST", f"/passes/{p['id']}/revoke")
    await m.answer(t(m.from_user, "refunded"))


@dp.message(Command("paysupport"))
async def support(m: Message):
    await m.answer(t(m.from_user, "paysupport"))


@dp.errors()
async def failed(event):
    if msg := getattr(event.update, "message", None):
        await msg.answer(t(msg.from_user, "failed", e=event.exception))


if __name__ == "__main__":
    asyncio.run(dp.start_polling(Bot(os.environ["TG_BOT_TOKEN"])))
