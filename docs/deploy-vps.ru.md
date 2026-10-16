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

Если у провайдера есть свой файрвол в панели управления, откройте UDP и TCP 443 и там.

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
