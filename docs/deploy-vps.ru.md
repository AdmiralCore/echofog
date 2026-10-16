English version: [deploy-vps.md](deploy-vps.md)

# Нода на VPS через Docker

Подойдёт любой VPS с публичным IP и Ubuntu или Debian. Здесь короткий рецепт; что значат роли и
настройки — в [Своя нода](run-a-node.ru.md).

## 1. Подключитесь и поставьте Docker

```bash
ssh root@<IP сервера>
```

```bash
curl -fsSL https://get.docker.com | sh
```

Если заходите не под root, а обычным пользователем, добавьте себя в группу `docker` и перезайдите:
`sudo usermod -aG docker "$USER"`.

## 2. Откройте порт

Нода использует один порт и для UDP (QUIC), и для TCP (запасной путь для сетей, где режут UDP).
Compose-файл публикует его как **443**. Если на сервере `ufw`:

```bash
ufw allow 443/udp && ufw allow 443/tcp
```

Если у провайдера есть свой файрвол в панели управления, откройте UDP и TCP 443 и там. **Главное —
UDP 443**: по нему идёт QUIC, а через него почти весь трафик, тогда как в облачных файрволах обычно
открыт только TCP. В Google Cloud:

```bash
gcloud compute firewall-rules create echofog --allow udp:443,tcp:443
```

## 3. Скачайте код и настройте

```bash
git clone https://github.com/AdmiralCore/EchoFog.git
cd EchoFog/echofog-core-oss
cp deploy/.env.example deploy/.env
nano deploy/.env
```

Главные настройки:

```bash
# только сигнальная нода (DHT + помощь с пробивкой NAT, без трафика) — дёшево; false — полноценный релей
ECHOFOG_SIGNALING=false
ECHOFOG_RELAY=true
# раздавать подключение сервера как платный выход (чужой трафик выходит с его IP)
ECHOFOG_EXIT=false
ECHOFOG_ADVERTISE_PORT=443
# пусто для самой первой ноды; иначе адрес подключения уже работающей ноды
ECHOFOG_BOOTSTRAP=
```

## 4. Запустите

```bash
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs -f
```

При первом запуске нода печатает **фразу восстановления** — запишите её и храните не в сети, она
восстанавливает личность и кошелёк ноды. Ещё она печатает **адрес подключения** (Join address):
`<this server's public IP or domain>:443/<peer id>`. Подставьте IP сервера — этот адрес другие
ноды указывают в `ECHOFOG_BOOTSTRAP`, а пользователи приложения вставляют в
**Настройки → Бутстрап-ноды**.

Проверьте, что всё в порядке:

```bash
docker compose -f deploy/docker-compose.yml ps
```

## Раздача интернета (выход)

Чтобы через сервер можно было ходить в интернет другим людям (или только вашим устройствам),
задайте в `deploy/.env`:

```bash
ECHOFOG_EXIT=true
ECHOFOG_EXIT_PRICE=0.05          # долларов за ГБ; 0 = бесплатно для всех
ECHOFOG_EXIT_UPLOAD_MBPS=100     # примерно столько, сколько есть у сервера; на новом сервере не 0
# ваши устройства ходят бесплатно и без дневного лимита (ID пиров — в Настройках приложения)
ECHOFOG_EXIT_FREE_PEERS=<peer id>,<peer id>
```

При `ECHOFOG_EXIT_UPLOAD_MBPS=0` нода сначала меряет скорость о других пиров; на новом сервере их
часто ещё нет, и в логе появляется `Exit sharing not started: no peers to measure upload speed
against`. На платном выходе каждому, кого нет в бесплатном списке, даётся 512 КБ пробного трафика
в день, дальше — только за оплату.

Строка статистики в логе считает отказы по причинам, например `exit: 12 sessions, 30 refused
(payment required 28, dial failed 2)`, а каждый отказ пишется как `refused stream` (не чаще раза
в 30 секунд на причину). Приложение на странице Прокси показывает, почему выход его не
обслуживает: выход выключен, бесплатный трафик кончился или выход не отвечает.

## TCP-обход

Нода принимает TCP, только пока маскируется под настоящий HTTPS-сайт, правдоподобный для её IP.
Она сама ищет такой сайт в своей подсети, но внутри Docker это часто не получается — тогда она
работает только по UDP и пишет в лог предупреждение `ANTI-DPI`. Чтобы включить TCP, укажите в
`deploy/.env` настоящий сайт на TLS 1.3 из того же дата-центра и его IP, затем снова выполните
`up -d`:

```bash
ECHOFOG_TLS_SERVER_NAME=<домен сайта>
ECHOFOG_CAMOUFLAGE_ADDR=<IP сайта>:443
```

Подробнее — [Anti-DPI](anti-dpi.md) (пока на английском).

## Обновление

```bash
cd ~/EchoFog && git pull
cd echofog-core-oss && docker compose -f deploy/docker-compose.yml up -d --build
```

Личность, база и кошелёк лежат в томе `echofog-data` и переживают обновления.
