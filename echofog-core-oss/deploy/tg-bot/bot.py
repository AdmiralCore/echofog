"""A Telegram bot that hands out passes of a hidden EchoFog node, and can sell them for Stars.

/pass NAME [DAYS]   (admins) a pass for a friend, no end unless DAYS; again: adds DAYS
/revoke ID          (admins) stop a pass and drop its devices
/list               (admins) every pass
/refund USER CHARGE (admins) give the Stars back and revoke that user's pass
/buy                TG_DAYS days for TG_STARS_PRICE Stars (off while the price is 0)
/my                 your own pass and its link
See README.md next to this file. The texts people see are in Russian; change them freely.
"""
import asyncio
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
STATES = {"active": "действует", "expired": "срок вышел", "used_up": "трафик кончился", "revoked": "отозван"}
dp = Dispatcher()


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


def card(p):
    term = "до " + datetime.fromtimestamp(p["expires"]).strftime("%d.%m.%Y") if p.get("expires") else "бессрочно"
    state = STATES.get(p["state"], p["state"])
    return (f"{p['label']}: {state}, {term}, устройств {len(p['devices'])} из {p['maxDevices']}\n"
            f"ID {p['id']}\n\n<code>{p['link']}</code>\n\n"
            "В EchoFog: Прокси → Конкретная нода → вставьте ссылку.")


def admin(m: Message):
    return m.from_user.id in ADMINS


@dp.message(Command("start", "help"))
async def start(m: Message):
    text = "/my — ваш пропуск и ссылка"
    if PRICE:
        text = f"/buy — {DAYS:g} дней VPN за {PRICE} ⭐\n{text}\n/paysupport — вопрос по оплате"
    if admin(m):
        text += "\n\nАдмин: /pass ИМЯ [ДНЕЙ], /revoke ID, /list, /refund USER CHARGE"
    await m.answer(text)


@dp.message(Command("pass"), admin)
async def give(m: Message, command: CommandObject):
    args = (command.args or "").split()
    if not args:
        return await m.answer("/pass ИМЯ [ДНЕЙ]")
    days = float(args[1]) if len(args) > 1 else 0
    await m.answer(card(await grant("friend:" + args[0], days)), parse_mode="HTML")


@dp.message(Command("revoke"), admin)
async def revoke(m: Message, command: CommandObject):
    await m.answer(card(await call("POST", f"/passes/{command.args}/revoke")), parse_mode="HTML")


@dp.message(Command("list"), admin)
async def listing(m: Message):
    passes = await call("GET", "/passes")
    lines = [f"{p['id']}  {STATES.get(p['state'], p['state'])}  {p['label']}" for p in passes]
    await m.answer("\n".join(lines) or "Пропусков пока нет.")


@dp.message(Command("my"))
async def my(m: Message):
    p = await pass_of(f"tg:{m.from_user.id}")
    await m.answer(card(p) if p else "У вас пока нет пропуска." + (" /buy" if PRICE else ""), parse_mode="HTML")


@dp.message(Command("buy"))
async def buy(m: Message, bot: Bot):
    if not PRICE:
        return await m.answer("Здесь пропуска не продаются.")
    await bot.send_invoice(m.chat.id, title="EchoFog VPN", description=f"{DAYS:g} дней, устройств: {DEVICES}",
                           payload="pass", currency="XTR", prices=[LabeledPrice(label="VPN", amount=PRICE)])


@dp.pre_checkout_query()
async def checkout(q: PreCheckoutQuery):
    await q.answer(ok=True)


@dp.message(F.successful_payment)
async def paid(m: Message, bot: Bot):
    p = await grant(f"tg:{m.from_user.id}", DAYS)
    await m.answer("Спасибо! " + card(p), parse_mode="HTML")
    for a in ADMINS:  # what /refund needs, if it comes to that
        await bot.send_message(a, f"Оплата: /refund {m.from_user.id} {m.successful_payment.telegram_payment_charge_id}")


@dp.message(Command("refund"), admin)
async def refund(m: Message, command: CommandObject, bot: Bot):
    args = (command.args or "").split()
    if len(args) != 2:
        return await m.answer("/refund USER CHARGE (их присылает бот при каждой оплате)")
    await bot.refund_star_payment(int(args[0]), args[1])
    if p := await pass_of(f"tg:{args[0]}"):
        await call("POST", f"/passes/{p['id']}/revoke")
    await m.answer("Звёзды возвращены, пропуск отозван.")


@dp.message(Command("paysupport"))
async def support(m: Message):
    await m.answer("Напишите владельцу бота: оплату можно вернуть.")


@dp.errors()
async def failed(event):
    if msg := getattr(event.update, "message", None):
        await msg.answer(f"Не получилось: {event.exception}")


if __name__ == "__main__":
    asyncio.run(dp.start_polling(Bot(os.environ["TG_BOT_TOKEN"])))
