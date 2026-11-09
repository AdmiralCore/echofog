package servernode

import (
	"fmt"
	"sort"
)

// msg is one operator-facing text in both languages. The Russian text is in the neutral form: no
// forms that depend on the reader's gender, no "вы".
type msg struct{ ru, en string }

var messages = map[string]msg{
	// ---- setup wizard
	"wizard.title":              {"Мастер установки сервера EchoFog v2. Enter — ответ по умолчанию, он указан в скобках.", "EchoFog v2 server setup. Press Enter to take the default shown in brackets."},
	"wizard.lang":               {"Язык (ru/en)", "Language (ru/en)"},
	"wizard.preset":             {"Роль сервера:\n  1) ретранслятор\n  2) бесплатный выход\n  3) домашний вход\n  4) платный выход — пока недоступно\n  5) приватный выход — пока недоступно\nНомер", "Server role:\n  1) relay\n  2) free exit\n  3) home entry\n  4) paid exit — not available yet\n  5) private exit — not available yet\nNumber"},
	"wizard.preset-unavailable": {"Эта роль пока недоступна в этой сборке: нужны оплата (T10.3) или скрытый режим. Нужно выбрать другую.", "This role is not available in this build yet: it needs payments (T10.3) or the hidden mode. Choose another one."},
	"wizard.domain":             {"Домен для своей обложки (пусто — без домена, нода только для своих)", "Domain for the node's own cover (empty: no domain, the node is for your own people only)"},
	"wizard.domain-invalid":     {"Это не похоже на доменное имя.", "That does not look like a domain name."},
	"wizard.site-dir":           {"Папка с содержимым сайта для обложки (пусто — без содержимого)", "Folder with the site content for the cover (empty: no content)"},
	"wizard.port":               {"UDP-порт ноды", "UDP port of the node"},
	"wizard.port-invalid":       {"Порт — число от 1 до 65535.", "The port is a number from 1 to 65535."},
	"wizard.address":            {"Публичный IP-адрес сервера (IP-литерал, не имя)", "Public IP address of this server (an IP literal, not a name)"},
	"wizard.address-invalid":    {"Нужен IPv4- или IPv6-адрес, не имя и не нулевой адрес.", "An IPv4 or IPv6 address is needed, not a name and not an unspecified address."},
	"wizard.data-dir":           {"Папка данных (ключи и база)", "Data folder (keys and database)"},
	"wizard.summary":            {"Итог:", "Summary:"},
	"wizard.start":              {"Запустить ноду сейчас? (y/n)", "Start the node now? (y/n)"},
	"wizard.bad-answer":         {"Ответ не принят, нужно ввести его ещё раз.", "That answer was not accepted, try again."},
	"wizard.line-role":          {"Роль: %s", "Role: %s"},
	"wizard.line-roles":         {"Функции: %s", "Functions: %s"},
	"wizard.line-domain":        {"Домен обложки: %s", "Cover domain: %s"},
	"wizard.line-address":       {"Адрес и порт в карточке: %s:%d", "Address and port in the card: %s:%d"},
	"wizard.line-data":          {"Данные: %s", "Data: %s"},
	"wizard.none":               {"нет", "none"},
	// ---- notes of the presets
	"preset.no-domain":           {"Без домена нода работает только для своих: публичной карточки и обложки нет.", "Without a domain the node works for your own people only: it has no public card and no cover."},
	"preset.ddns":                {"Бесплатный DDNS работает, но заметнее и рискует общей блокировкой всего сервиса. Лучше собственный домен.", "A free DDNS name works, but it is more conspicuous and risks a block of the whole service. An own domain is better."},
	"preset.empty-site":          {"Папка с сайтом не указана: обложке нечего показывать. Общего шаблона нет — нужен собственный сайт.", "No site folder was given: the cover has nothing to show. There is no shared template — an own site is needed."},
	"preset.acme":                {"Сертификат обложки будет выпущен автоматически (ACME). Для проверки домен должен указывать на этот сервер, а TCP-порт 443 быть открыт.", "The cover certificate is issued automatically (ACME). For validation the domain must point at this server and TCP port 443 must be open."},
	"preset.exit-responsibility": {"Выход открывает интернет-соединение сервера другим людям: ответственность за их трафик перед хостером и законом остаётся на операторе.", "An exit shares this server's internet connection with other people: responsibility for their traffic towards the host and the law stays with the operator."},
	"preset.home-entry-wired":    {"Домашний вход годится для проводных сетей. Для мобильных сетей с белыми списками нужен адрес из проверенного списка (T15.2a, T12.5).", "A home entry suits wired networks. For mobile networks with allow-lists the address must be in a verified list (T15.2a, T12.5)."},
	"preset.home-entry-verified": {"Адрес входит в проверенные диапазоны файла правил: такой вход годится и для мобильных сетей с белыми списками (после проверки куратором с SIM).", "The address is inside the verified ranges of the rules file: such an entry also suits mobile networks with allow-lists (once the curator has checked it with a SIM)."},
	"preset.home-entry-risk":     {"Риски оператора домашнего входа: нода видит адреса людей, которые ею пользуются, и объём их трафика (содержимое сообщений шифруется сквозным шифрованием); к оператору могут прийти с требованием раскрыть данные; законы страны могут ограничивать такую деятельность, это не является юридической консультацией. Снизить риск помогают: отдельный сервер без личных данных, выход выключен, минимум журналов, публичный контакт только для жалоб. Подробности: docs/domestic-entry.ru.md.", "Risks for the operator of a home entry: the node sees the addresses of the people who use it and the volume of their traffic (message content is end-to-end encrypted); the operator may be asked to hand over data; the laws of the country may restrict this activity, and this is not legal advice. What lowers the risk: a separate server with no personal data, the exit switched off, minimal logs, a public contact for complaints only. Details: docs/domestic-entry.md."},
	// ---- config file comments
	"config.header":   {"Конфигурация сервера EchoFog v2. Строки, начинающиеся с //, — комментарии.", "EchoFog v2 server configuration. Lines that start with // are comments."},
	"config.listen":   {"listen: носитель (noise3, reality2-quic, reality2-tcp, ws2), адрес приёма и IP-адрес, который попадает в карточку. Замаскированным носителям нужны cover и site_dir.", "listen: the carrier (noise3, reality2-quic, reality2-tcp, ws2), the bind address and the IP address written into the card. Camouflaged carriers need cover and site_dir."},
	"config.roles":    {"roles: relay, mailbox, dht-server, exit. Пустой список выключает все роли.", "roles: relay, mailbox, dht-server, exit. An empty list switches every role off."},
	"config.cover":    {"cover и site_dir: домен обложки и папка с её сайтом. cover_cert — свой сертификат (PEM-бандл); пусто — сертификат выпускается через ACME, для этого нужен cover_acme.accept_tos. Без обложки публичной карточки нет.", "cover and site_dir: the cover domain and the folder of its site. cover_cert is an own certificate (PEM bundle); empty means ACME, which needs cover_acme.accept_tos. Without a cover there is no public card."},
	"config.operator": {"operator: API оператора слушает только 127.0.0.1 или ::1, токен лежит в operator.token.", "operator: the operator API listens on 127.0.0.1 or ::1 only; the token is in operator.token."},
	// ---- doctor
	"doctor.config.ok":          {"Конфигурация корректна.", "The configuration is valid."},
	"doctor.config.bad":         {"Конфигурация не проходит проверку: %s. Нужно исправить server.json.", "The configuration does not validate: %s. Fix server.json."},
	"doctor.files.ok":           {"Права на файлы ключей и каталог данных в порядке.", "Permissions of the key files and the data folder are fine."},
	"doctor.files.loose":        {"Файл или каталог доступен посторонним: %s. Нужно выполнить chmod 600 для файлов и chmod 700 для каталога.", "A file or folder is open to others: %s. Run chmod 600 on files and chmod 700 on the folder."},
	"doctor.port.ok":            {"Порт %s свободен для ноды.", "Port %s is free for the node."},
	"doctor.port.node":          {"Порт %s занят самой нодой (она запущена).", "Port %s is in use by the node itself (it is running)."},
	"doctor.port.busy":          {"Порт %s занят другим процессом: %s. Нужно остановить его или выбрать другой порт.", "Port %s is used by another process: %s. Stop it or choose another port."},
	"doctor.reach.ok":           {"К ноде подключены пиры: %d.", "Peers are connected to the node: %d."},
	"doctor.reach.none":         {"Пиры пока не подключены. Нужно проверить, что UDP-порт открыт в файрволе и в панели хостера. Достижимость снаружи определяется по подключению пира, внешние сервисы не используются.", "No peers have connected yet. Check that the UDP port is open in the firewall and in the host's panel. Reachability from outside is judged by a peer connecting; no outside service is used."},
	"doctor.reach.skipped":      {"Нода не запущена: достижимость не проверена.", "The node is not running: reachability was not checked."},
	"doctor.dns.ok":             {"DNS домена %s указывает на этот IP-адрес.", "DNS of %s points at this IP address."},
	"doctor.dns.mismatch":       {"DNS домена %s указывает на другой адрес. Нужно задать A/AAAA-запись на %s.", "DNS of %s points elsewhere. Set the A/AAAA record to %s."},
	"doctor.dns.partial":        {"DNS домена %s содержит нужный адрес и посторонние. Часть клиентов попадёт не на эту ноду; лишние записи нужно убрать.", "DNS of %s has this address and others. Some clients will not reach this node; remove the extra records."},
	"doctor.dns.none":           {"Для домена %s не удалось получить записи DNS. Нужно создать A/AAAA-запись на %s.", "No DNS records could be fetched for %s. Create an A/AAAA record for %s."},
	"doctor.dns.skipped":        {"DNS не проверялся: по умолчанию doctor ничего не отправляет наружу. Ключ --dns выполнит поиск домена через системный резолвер.", "DNS was not checked: by default doctor sends nothing out. The --dns flag looks the domain up through the system resolver."},
	"doctor.dns.nodomain":       {"Домен обложки не задан, проверять DNS нечего.", "No cover domain is set, so there is no DNS to check."},
	"doctor.cert.ok":            {"Сертификат обложки действителен ещё %d дн.", "The cover certificate is valid for %d more days."},
	"doctor.cert.soon":          {"Сертификат обложки истекает через %d дн. Продлить его нужно сейчас.", "The cover certificate expires in %d days. Renew it now."},
	"doctor.cert.expired":       {"Сертификат обложки истёк или ещё не действует. Нужно выпустить новый.", "The cover certificate has expired or is not valid yet. Issue a new one."},
	"doctor.cert.unreadable":    {"Файл сертификата обложки не читается или не является сертификатом (%s).", "The cover certificate file cannot be read or is not a certificate (%s)."},
	"doctor.cert.skipped":       {"Сертификат обложки не задан.", "No cover certificate is set."},
	"doctor.clock.ok":           {"Часы в порядке (предупреждений ноды нет).", "The clock is fine (the node reports no warning)."},
	"doctor.clock.warn":         {"Нода сообщает о расхождении часов. Нужно включить синхронизацию времени (chrony, systemd-timesyncd).", "The node reports a clock difference. Enable time synchronisation (chrony, systemd-timesyncd)."},
	"doctor.clock.unknown":      {"Часы не проверены: нода не запущена.", "The clock was not checked: the node is not running."},
	"doctor.disk.ok":            {"Свободного места: %d МиБ.", "Free disk space: %d MiB."},
	"doctor.disk.low":           {"Свободного места мало: %d МиБ. Нужно освободить место.", "Free disk space is low: %d MiB. Free some space."},
	"doctor.disk.full":          {"Свободного места почти нет: %d МиБ. База может повредиться, место нужно освободить немедленно.", "Almost no free disk space: %d MiB. The database may be damaged; free space immediately."},
	"doctor.disk.unknown":       {"Свободное место не измерено на этой платформе.", "Free disk space was not measured on this platform."},
	"doctor.version":            {"Версия: %s.", "Version: %s."},
	"doctor.cover.none":         {"Обложка не задана: у ноды нет публичной карточки, она обслуживает только своих.", "No cover is set: the node has no public card and serves only its own people."},
	"doctor.cover.ok":           {"Обложка %s обслуживается из папки %s.", "The cover %s is served from the folder %s."},
	"doctor.cover.missing":      {"Замаскированный носитель настроен без cover: незнакомцу нечего показать. Нужно задать cover и site_dir.", "A camouflaged listener is configured without a cover: there is nothing to show a stranger. Set cover and site_dir."},
	"doctor.cover.site-missing": {"Обложка %s задана без site_dir. Пока нет T2.6b-3, чужой сайт не раздаётся: нужен собственный сайт оператора (site_dir).", "The cover %s is set without site_dir. Until T2.6b-3 a foreign site is not served: the operator's own site (site_dir) is required."},
	"doctor.summary.ok":         {"Проблем не найдено.", "No problems found."},
	"doctor.summary.warn":       {"Есть замечания: %d.", "There are remarks: %d."},
	"doctor.summary.fail":       {"Есть ошибки: %d.", "There are errors: %d."},
	"doctor.level.ok":           {"В ПОРЯДКЕ", "OK"},
	"doctor.level.info":         {"ИНФО", "INFO"},
	"doctor.level.warn":         {"ВНИМАНИЕ", "WARN"},
	"doctor.level.fail":         {"ОШИБКА", "FAIL"},
	// ---- status and update
	"status.title":            {"Состояние ноды EchoFog v2", "EchoFog v2 node status"},
	"update.available":        {"Доступна версия %s (файл %s, %d Б).", "Version %s is available (file %s, %d bytes)."},
	"update.rollback-warning": {"ВНИМАНИЕ: это откат с версии %s на %s. Старые версии могут содержать исправленные уязвимости. Откат выполняется только по явному флагу и не ниже min_version подписанного манифеста.", "WARNING: this is a rollback from %s to %s. Older versions may carry fixed vulnerabilities. A rollback happens only with the explicit flag and never below the min_version of the signed manifest."},
	"update.none":             {"Установлена последняя версия.", "This is the latest version."},
	"update.confirm":          {"Установить версию %s? Бинарь будет заменён, предыдущий сохранится рядом. (y/n)", "Install version %s? The binary will be replaced and the previous one kept beside it. (y/n)"},
	"update.done":             {"Версия %s установлена; нужен перезапуск ноды (restart), чтобы она заработала.", "Version %s is installed; restart the node to use it."},
	"update.declined":         {"Обновление не подтверждено, ничего не изменено.", "The update was not confirmed; nothing was changed."},
	"update.no-keys":          {"В этой сборке нет ключей релиза (release keys): обновление невозможно. Нужна официальная сборка.", "This build has no release keys: it cannot be updated. Use an official build."},
	"update.docker":           {"В Docker образ обновляется заменой тега и перезапуском контейнера, а не командой update.", "In Docker the image is updated by changing the tag and restarting the container, not with update."},
}

// Message returns the operator-facing text for a code in a language ("ru" or "en"; anything else
// is English). An unknown code returns "".
func Message(lang, code string, args ...any) string {
	m, ok := messages[code]
	if !ok {
		return ""
	}
	text := m.en
	if lang == "ru" {
		text = m.ru
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// Codes lists every message code (tests check that each has both languages).
func Codes() []string {
	out := make([]string, 0, len(messages))
	for c := range messages {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
