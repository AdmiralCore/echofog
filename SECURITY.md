# Security

Русская версия — [ниже](#по-русски).

## Current status

EchoFog is **experimental**.

- It has **not** had an independent security audit by a specialist organization yet. This includes
  the TON vault contract, which runs on testnet only.
- It has **not** yet been tested against a real national DPI (such as Russia's TSPU or China's
  GFW). The anti-DPI design is described in [docs/anti-dpi.md](docs/anti-dpi.md), but the real test
  is only starting.
- Do not rely on it to protect you in a high-risk situation yet.

How the pieces fit together is explained in [docs/how-it-works.md](docs/how-it-works.md).

## What has been done so far

On 2026-10-15, before the first public test, the project ran an internal security pass over the
code. It is not a substitute for an independent audit, but it found real problems, and these were
fixed:

- **Paying with someone else's credit.** A replayed old payment voucher could let one user spend
  another user's prepaid credit at an exit. Now only a newer voucher, which only the owner of the
  vault can sign, is accepted.
- **Exits reaching their operator's own network.** A client could make a public exit connect to the
  operator's own public address (and through it the router's admin page), to devices on the
  operator's home network, or to reserved address ranges, and could use ports that mostly serve spam
  and worms. Exits now refuse all of these.
- **Remote crash.** A malformed frame from any peer could crash the whole node. Bad frames are now
  dropped.
- **Memory exhaustion.** A peer could announce very large frames, even before authenticating, and make
  the node reserve memory it never got data for. Frames are now read as they arrive, and handshake
  frames are kept small.
- **Lookup storms.** A DHT lookup could bounce between two peers until it timed out, redialing them
  over and over. Lookups are now bounded.
- **Active-probing fingerprints.** A server node rate-limited and banned visitors without a valid
  token, which a censor's probe could notice. Such visitors are now treated exactly like visitors of
  the real site the node imitates. Server nodes no longer share one built-in imitated site name, and
  the length of their encrypted certificate no longer has a fixed, recognisable size.
- **DNS leak on Windows in whole-computer mode.** Windows could send DNS lookups to the ISP outside
  the tunnel. That is now switched off while the tunnel runs and restored afterwards. On Linux,
  outbound DNS is redirected into the tunnel on a best-effort basis.
- **Local attacks on the desktop app.** Profile deletion could be tricked into touching files
  outside the profiles folder (it now also requires the app password); the local status page could
  be reached from a web page through DNS rebinding; and the **Emergency lock** left a copy of the
  identity key in memory. All three are fixed. Large wallet sends now ask for confirmation, and
  logs mask IP addresses.

## Known limitations (still open)

- **No kill switch in whole-computer mode.** If EchoFog or its helper stops unexpectedly, apps fall
  back to your normal connection and your real IP can be exposed for a moment. The app shows this
  warning next to the switch.
- **The DHT lists public nodes.** Their address and ID are published together, so a censor who
  crawls the DHT can find them and confirm they are EchoFog nodes. The per-node disguise key only
  stops blind scanning. Hidden and isolated nodes are not listed and never tell other nodes their
  address: see [docs/hidden-nodes.md](docs/hidden-nodes.md).
- **Hidden nodes are new.** A hidden or isolated node answers only peers that prove its access
  secret (from its invitation link) in the handshake. Anyone who has the link can use it, so a
  leaked link has to be replaced (*New link* / `invite --rotate`). With obfuscation off
  (`--obfs off`) a scanner can still learn the node's ID from its TLS certificate, and the relays
  named in a hidden node's link change over time. Links are handed out by people; there is no
  automatic distribution.
- **Some anti-DPI work is deferred:** a client does not yet present each target node's own imitated
  site name, and there is no extra padding of the first UDP packets yet. See
  [docs/anti-dpi.md](docs/anti-dpi.md).
- **Payments are new.** The vault contract is on testnet only and unaudited; relays are not paid yet
  (only exits).

## Threat model, in plain words

What EchoFog tries to protect:

- **The content of your messages and calls.** They are end-to-end encrypted. Relays, mailboxes and
  other nodes in between see only ciphertext.
- **The protocol, from passive observers.** Someone watching the wire should see random-looking UDP
  or what looks like a browser talking to an ordinary HTTPS site. A probe that connects to a server
  node without the right token sees the real site that node imitates.
- **Data on your disk.** It is encrypted and opens only with your password or recovery phrase.

What it does **not** protect:

- **The fact that you use EchoFog, against a determined censor.** A censor who crawls the DHT can
  learn the addresses of public nodes and see that you connect to them. Hidden and isolated nodes
  are not in the DHT, but anyone who gets their invitation link can find and use them.
- **Your destinations, from the exit.** Like any VPN, the exit you use sees which sites you visit
  and can see unencrypted traffic. Use HTTPS. The exit may also see your IP address.
- **Payment metadata on TON.** The blockchain is public. Your vault, your deposits and the vouchers
  an exit collects are visible on-chain and can link your vault to the exits you paid.
- **A compromised computer.** If someone controls your device, they can read what you read.

## Reporting a vulnerability

Please report security problems **privately**, not in a public issue:

<!-- TODO(owner): contact -->

Tell us what you found, how to reproduce it and what an attacker could do with it. Please give the
maintainers reasonable time to fix it and ship the fix before you disclose it publicly. People in
censored countries will depend on that fix arriving before the details are out.

---

## По-русски

### Текущий статус

EchoFog — **экспериментальный** проект.

- У него **ещё не было** независимого аудита безопасности от специализированной организации. Это
  касается и контракта-хранилища в TON, который пока работает только в тестовой сети.
- Его **ещё не проверяли** против настоящего государственного DPI (ТСПУ в России, GFW в Китае).
  Как устроена защита от DPI, описано в [docs/anti-dpi.md](docs/anti-dpi.md), но настоящая проверка
  только начинается.
- В ситуациях с высоким риском на него пока полагаться нельзя.

Как всё устроено в целом — в [docs/how-it-works.ru.md](docs/how-it-works.ru.md).

### Что уже сделано

15 октября 2026 года, перед первым публичным тестом, команда проекта сама прошлась по коду с точки
зрения безопасности. Независимый аудит это не заменяет, но настоящие проблемы нашлись, и их
исправили:

- **Оплата чужим балансом.** Повторно отправленный старый чек мог позволить одному пользователю
  тратить предоплаченный баланс другого на выходном узле. Теперь принимается только более новый чек,
  а подписать его может только владелец хранилища.
- **Выходной узел пускал в сеть своего владельца.** Клиент мог заставить публичный выходной узел
  подключиться к публичному адресу самого владельца (а через него — к странице настроек роутера), к
  устройствам в его домашней сети или к зарезервированным диапазонам адресов, а также к портам,
  которые нужны в основном для спама и червей. Теперь всё это запрещено.
- **Удалённое падение.** Испорченный кадр от любого узла мог уронить всю ноду. Теперь такие кадры
  просто отбрасываются.
- **Исчерпание памяти.** Узел мог объявить огромные кадры (даже до аутентификации) и заставить ноду
  выделять память под данные, которые так и не приходили. Теперь кадры читаются по мере поступления,
  а кадры рукопожатия ограничены по размеру.
- **Шторм запросов в DHT.** Поиск в DHT мог гонять запрос между двумя узлами до таймаута,
  переподключаясь к ним снова и снова. Теперь поиск ограничен.
- **Отпечатки при активном прощупывании.** Серверная нода ограничивала и банила посетителей без
  правильного токена — зонд цензора мог это заметить. Теперь с ними обращаются точно так же, как
  настоящий сайт, под который маскируется нода. У серверных нод больше нет общего встроенного имени
  сайта для маскировки, а длина их зашифрованного сертификата больше не имеет узнаваемого
  фиксированного размера.
- **Утечка DNS на Windows в режиме «весь компьютер».** Windows могла отправлять DNS-запросы
  провайдеру мимо туннеля. Теперь на время работы туннеля это отключается, а потом возвращается как
  было. На Linux исходящий DNS перенаправляется в туннель по мере возможности.
- **Локальные атаки на десктопное приложение.** Удаление профиля можно было обманом направить на
  файлы за пределами папки профилей (теперь оно ещё и требует пароль приложения); до локальной
  страницы статуса можно было достучаться с веб-страницы через DNS rebinding; а **Экстренная
  блокировка** оставляла в памяти копию ключа личности. Все три исправлены. Кроме того, крупные
  переводы из кошелька теперь требуют подтверждения, а в логах IP-адреса скрыты.

### Известные ограничения (пока не решены)

- **Нет аварийного отключения (kill switch) в режиме «весь компьютер».** Если EchoFog или его
  помощник внезапно остановятся, программы вернутся к обычному соединению, и ваш настоящий IP может
  на мгновение раскрыться. Приложение предупреждает об этом рядом с переключателем.
- **DHT публикует публичные ноды.** Их адрес и ID лежат там вместе, так что цензор, который обходит
  DHT, может их найти и убедиться, что это ноды EchoFog. Ключ маскировки, привязанный к ноде,
  защищает только от слепого сканирования. Скрытые и изолированные ноды в DHT не публикуются и
  никогда не сообщают другим нодам свой адрес: см. [docs/hidden-nodes.ru.md](docs/hidden-nodes.ru.md).
- **Скрытые ноды — новинка.** Скрытая или изолированная нода отвечает только тем, кто в
  рукопожатии доказал знание её секрета доступа (из ссылки-приглашения). Пользоваться ею может
  любой, у кого есть ссылка, поэтому утёкшую ссылку надо заменить (*Новая ссылка* /
  `invite --rotate`). С выключенной обфускацией (`--obfs off`) сканер всё ещё может узнать ID ноды
  по её TLS-сертификату, а релеи, указанные в ссылке скрытой ноды, со временем меняются. Ссылки
  раздают люди, автоматической раздачи нет.
- **Часть работы против DPI отложена:** клиент пока не предъявляет имя сайта, под который
  маскируется именно та нода, к которой он подключается, и нет дополнительного выравнивания длины
  первых UDP-пакетов. См. [docs/anti-dpi.md](docs/anti-dpi.md).
- **Платежи совсем новые.** Контракт-хранилище работает только в тестовой сети и не проходил аудит;
  релеям пока не платят (только выходным узлам).

### Модель угроз простыми словами

Что EchoFog старается защитить:

- **Содержимое сообщений и звонков.** Оно зашифровано end-to-end. Релеи, почтовые ящики и другие
  ноды по дороге видят только шифротекст.
- **Сам протокол — от пассивного наблюдателя.** Тот, кто смотрит на трафик, должен видеть
  случайный на вид UDP или то, что похоже на браузер, открывающий обычный HTTPS-сайт. Зонд, который
  подключается к серверной ноде без правильного токена, видит настоящий сайт, под который она
  маскируется.
- **Данные на диске.** Они зашифрованы и открываются только паролем или фразой восстановления.

Что он **не** защищает:

- **Сам факт, что вы пользуетесь EchoFog, — от настойчивого цензора.** Цензор, который обходит DHT,
  может узнать адреса публичных нод и увидеть, что вы к ним подключаетесь. Скрытых и изолированных
  нод в DHT нет, но любой, к кому попала их ссылка-приглашение, может их найти и ими пользоваться.
- **Куда вы ходите — от выходного узла.** Как и любой VPN, выходной узел видит, какие сайты вы
  открываете, и может видеть незашифрованный трафик. Пользуйтесь HTTPS. Выходной узел может видеть и
  ваш IP-адрес.
- **Метаданные платежей в TON.** Блокчейн публичный. Ваше хранилище, депозиты и чеки, которые
  собирает выходной узел, видны в сети и могут связать ваше хранилище с узлами, которым вы платили.
- **Взломанный компьютер.** Если кто-то управляет вашим устройством, он прочитает всё, что читаете
  вы.

### Как сообщить об уязвимости

Пожалуйста, сообщайте о проблемах безопасности **приватно**, а не в публичном issue:

<!-- TODO(owner): contact -->

Опишите, что нашли, как это воспроизвести и что с этим может сделать атакующий. Дайте, пожалуйста,
мейнтейнерам разумное время, чтобы исправить проблему и выпустить исправление, прежде чем
рассказывать о ней публично. Люди в странах с цензурой будут зависеть от того, чтобы исправление
пришло раньше подробностей.
