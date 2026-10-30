# Usernames from crypto naming systems

Status: design + resolver prototype (`internal/names`). Nothing is wired into the node API or the
desktop yet. October 2026.

## The question

Today a person is found by a 64-character key, a `fogcontact1:` link or its https form. Can people
instead be found by a readable name they own in a blockchain naming system, such as `alice.ton` on
TON or `alice.sol` on Solana, without a central EchoFog server?

## Recommendation

**Start with TON, read-only first, then let people link their name from the app.**

- One resolver reads both `.ton` names (TON DNS) and Telegram usernames (`alice.t.me`, which are
  NFTs sold on Fragment and answer the same `dnsresolve` interface). Many prospective users already
  own a Fragment username; that is the strongest reason to start here.
- The core already talks to TON liteservers through `tonutils-go` for the wallet, with config
  caching and proof checks on account state. No new dependency, no API key, no HTTP gateway.
- The desktop already has a TON wallet, and monetisation is on TON, so linking (and later buying) a
  name can happen in the app with the user's confirmation.
- Cost is low: a long `.ton` name starts at about 1 TON and costs about 0.015 TON a year to keep.

Solana (`.sol`) is worth adding later as a read-only network if users ask for it. The same record
format works there. EchoFog has no Solana wallet, reads would depend on a few HTTP RPC providers
with no proofs, and that weakens the "no central server" story. ENS (`.eth`) is not worth it now:
it costs a yearly fee plus Ethereum L1 gas, it is mid-migration to ENSv2, and a trustless read
needs an Ethereum light client that Go code does not have.

A name is only ever a **way to find a key**. Once someone is added as a contact, the contact is
the key, as today. A name that later points elsewhere never silently changes a contact.

## The options

| | TON DNS `.ton` | Telegram `.t.me` (Fragment) | Solana Name Service `.sol` | ENS `.eth` |
|---|---|---|---|---|
| Price | Auction; long names (11+ chars) from ~1 TON, short ones far more | Auction on Fragment; usually tens to thousands of TON, 5% fee on resale | ~$20 one-time for 5+ characters, more for shorter (USDC, wSOL and others) | $5/year for 5+ characters ($160 and $640 for 4 and 3), plus L1 gas |
| Keeping it | Any owner transaction once a year (~0.015 TON); after a year without one, anyone can release it to a new auction | No yearly fee | No renewal | Yearly renewal |
| Ownership | NFT (TEP-62) owned by a TON wallet | NFT owned by a TON wallet | Name account owned by a Solana key (or a tokenised NFT) | ERC-721/1155 owned by an Ethereum account |
| Arbitrary record | Yes: any 256-bit category, `dns_text` value (TEP-81) | Same interface (to confirm that the item contract accepts `change_dns_record` for a custom category) | Records v2 `TXT`, with a staleness field tied to the owner | Text records with any key (ENSIP-5) |
| Read without a server | Liteservers over ADNL; account state is proof-checked (see "Trust in the answer") | Same | JSON-RPC `getAccountInfo` on a public or paid RPC; no proofs | `eth_call` through the Universal Resolver on an RPC; trustless only with a light client (Helios, Rust); off-chain names need CCIP-read gateways |
| Blocking risk | A few dozen liteserver IPs, config from ton.org (cached) | Same | A handful of RPC hostnames; the free public one is rate-limited | Infura/Alchemy-style providers; gateways for off-chain names |
| In-app purchase | Feasible: the wallet exists | Fragment's site only | Needs a Solana wallet | Needs an Ethereum wallet and gas |

Others were not considered further: Unstoppable Domains (resolution and minting depend on the
company's infrastructure), Namecoin and Handshake (tiny user base, and they need their own chain
clients).

### TON DNS details

- `dnsresolve(name_slice, category)` runs on the root contract (address in config param 4), which
  hands off to the `.ton` collection, which hands off to the name's NFT item (or to a subdomain
  resolver). `tonutils-go/ton/dns` implements the walk; `.t.me` is a delegated sub-resolver of the
  same root.
- Records are a dictionary `sha256(category name) → ^DNSRecord`. Standard categories are `wallet`,
  `site`, `storage` and `dns_next_resolver`; any other 256-bit key is allowed. TEP-81 defines
  `dns_text#1eda _:Text`, a chunked string.
- Records are written by the owner sending `change_dns_record` (op `0x4eb1f0f9`, query id, 256-bit
  key, value as a ref) to the NFT item. `tonutils-go` builds the body
  (`Domain.BuildSetRecordPayload`).
- A `.ton` name lapses if its owner sends nothing to it for a year, and then goes back to auction.
  Records may survive a change of owner, which is why our record names the owner it was made for.

### Solana Name Service details

- A name is an account derived from `sha256("SPL Name Service" + name)` under the `.sol` parent.
  Records v2 are child accounts (`"\x02" + record name`) owned by the SNS records program, with a
  header: staleness validation (u16), right-of-association validation (u16), content length (u32),
  then the staleness id (the owner's key for Solana-owned names) and the content.
- Staleness is built in: a record whose staleness id is not the current owner is stale.
  Right-of-association is only defined for SOL, ETH, BSC, INJ, BASE, URL and CNAME. `TXT` has no
  authenticity check of its own, so the EchoFog signature below is still needed.
- A client reads it with two `getAccountInfo` calls, or through Bonfida's HTTP proxy (a central
  service; avoid). There is no light client, so the answer is as honest as the RPC.

### ENS details

- Text records can carry any key (`org.echofog.binding` would do). Reading needs an Ethereum RPC.
  Names under off-chain resolvers (CCIP-read, used by many L2 subnames) also need the resolver's
  HTTPS gateway.
- ENS Labs dropped its own L2 (Namechain) in early 2026 and is moving ENSv2 to L1. Registration and
  record changes cost L1 gas. Revisit if users bring `.eth` names and a Go light client appears.

## Design

### What a name record says

The name's owner stores one text record. The record binds the name to an EchoFog identity, and the
identity signs it:

```
fogname1:<base64url( body || signature )>

body = version (1 byte, = 1)
     | issued: unix seconds (8, big endian)
     | identity public key (32): the key in contact links; the peer ID is its SHA-256
     | owner length (1) | owner (n ≤ 64): the name's owner when the record was made
                                         (TON: workchain byte + 32-byte account; Solana: 32-byte key)
signature = Ed25519 by the identity over
     "echofog name binding v1\0" || normalised name || "\0" || body
```

About 190 characters. On TON the record is a `dns_text` under category `sha256("echofog")`; it
spans two cells. On Solana it would be the content of a Records v2 `TXT`.

The record carries the key, not a whole contact link. Addresses and relays change, and the network
finds a peer by its ID; a key also keeps the record small and stable.

What the signature does and does not protect:

| Threat | Covered? |
|---|---|
| Someone points their name at *your* key without your consent (a slur name, a fake "official" name) | Yes: only your key can sign a binding to your key, and the binding names the name |
| A record copied from `alice.ton` into `alice2.ton` | Yes: the name is under the signature |
| A record left in the name after it is sold or lapses | Yes: the owner in the record no longer matches; it shows as **stale** |
| Whoever holds the name now (a buyer, or a thief of the owner's wallet) writes a record for *their own* key | **No.** The name belongs to them now. Defences: contacts are pinned to keys, a changed name is flagged, and the record's age is shown |
| A dishonest liteserver or RPC invents an answer | **No**, not by the signature. See "Trust in the answer" |

### Resolution states

`internal/names.Resolver.Resolve(ctx, name)` returns one of:

| Status | Meaning | What the UI shows |
|---|---|---|
| `verified` | Record present, signed by the identity it names, for this name, made by the current owner | Name, short key fingerprint, "linked N days ago", **Add contact** |
| `stale` | Record signed for an earlier owner (or nobody holds the name now) | "This name was linked by a previous owner. The current owner has not linked it." No add button |
| `no-record` | The name exists but holds no EchoFog record | "alice.ton exists but is not linked to EchoFog" |
| `not-found` | Nobody holds the name | "Nobody owns alice.ton" (later: "Get this name") |
| `invalid` | Unreadable record, a wrong signature, or the name's contract failed | "The EchoFog record in this name is broken or forged" |

An error means the chain could not be read and no recent answer is cached. A cached answer given
while offline is marked `Cached`, and the UI says when it was read.

### Name and key disagree

- **Searching a name, no contact with that key yet:** show `verified` as above. The key fingerprint
  is part of the card so that people who compare fingerprints can.
- **A saved contact was added through a name, and the name now gives another key (or is
  stale):** the contact is not changed. Its profile shows a warning: "alice.ton no longer points to
  this contact. The name may have changed hands." Messages keep going to the saved key.
- **A name resolves to a key that is already a contact under another name:** show the existing
  contact ("This is Bob, already in your contacts") instead of offering a duplicate.
- **A fresh binding on a name that used to point elsewhere:** if this device saw another key for
  the name before (in the cache, or in a contact's saved name), say so: "alice.ton pointed to a
  different identity until 3 days ago."

The contact stores the name it was found by and the record's `issued` time. The name is checked
again when the profile is opened, not in the background, so the app does not keep telling
liteservers which names the user cares about.

### Search and Open link (Ctrl+L)

- `frontend/src/lib/links.js`: `classifyLink` gains `{ kind: 'name', name }` for text that
  `names.IsName` accepts (`*.ton`, `*.t.me`, `*.sol`; ASCII only, so look-alike Cyrillic letters
  are rejected rather than resolved). `@alice` is not treated as `alice.t.me`, because `@` already
  means other things in chats.
- `OpenLink.svelte`: a name shows a "Look up alice.ton" button, and the lookup runs only when it is
  pressed (or Enter), never per keystroke, so half-typed names do not reach liteservers. The result
  is the card from the table above.
- The contacts search box shows the same "Look up" row when the query looks like a name.
- The https join page could later accept `#name/alice.ton`, but the app does the resolving; the
  page stays static.

### Caching

- In memory (prototype): `verified`, `stale` and `no-record` for 1 hour; `not-found` and `invalid`
  for 5 minutes; when the chain is unreachable, the last answer up to 7 days old, marked `Cached`.
  A "check again" action calls `Forget`.
- Later, a small on-disk cache in the profile (encrypted like other profile data), so the
  offline fallback survives a restart. Contacts themselves need no cache: they are keys.
- Concurrent lookups of the same name should share one chain read (singleflight) once the UI calls
  it.

### Trust in the answer (TON)

`tonutils-go` with the wallet's `ProofCheckPolicyFast` checks the proof of the *account state*
against the masterchain block the liteserver reports, but takes the *result of the get-method*
(`dnsresolve`, `get_nft_data`) from the liteserver as it is. A dishonest liteserver can therefore
invent a name's owner and record, and the signature does not help, because it can sign its own.

Steps, in order of cost:

1. Ask two liteservers from different operators and require the same answer (owner + record).
   Cheap, and it raises the bar from one server to two.
2. Read the item's account state (proved) and parse the NFT item data ourselves (owner, records
   dictionary) instead of trusting the get-method result. The `.ton` and `.t.me` item layouts are
   public. The item address is derived from the collection's item code and the name's index, both
   in proved state.
3. `ProofCheckPolicySecure` with a trusted key block from the config, so masterchain blocks are also
   checked. A one-time sync cost per start.

Pinning (above) limits the damage of any wrong answer to new contacts.

### Privacy

- **A name links a wallet to an EchoFog identity, publicly and permanently.** The record can be
  removed, but chain history keeps it. Anyone can then see what that wallet paid for and when, and
  which identity it belongs to.
- The built-in wallet pays for VPN access. If it also owns the name, VPN payments become
  attributable to the identity. The link flow must say so and suggest holding the name in a
  separate wallet. A name the built-in wallet owns gets a visible warning.
- Lookups tell the liteserver the client's IP and which name's contracts it reads. The liteclient
  dials TCP directly (`net.DialTimeout` in `tonutils-go`), so routing it through the user's
  SOCKS/Tor setting needs a dialer hook (a small fork or an upstream patch). Until then, lookups
  happen only on an explicit user action.
- Publishing a key makes it easy for strangers to message the person. The existing contact-request
  and hidden-node settings apply; the link flow should point to them.
- An identity can hold several names, or none. Nothing in EchoFog requires a name.

### Censorship and dependencies

- TON: the liteserver list comes from `ton.org/global.config.json`; the wallet already caches it.
  If ton.org or the liteserver IPs are blocked, lookups fail and cached answers are used. A later
  fallback can fetch the config and reach liteservers through an EchoFog exit, the same way as other
  traffic.
- Names themselves can be taken by the registry's rules: a `.ton` name lapses after a year without a
  renewal transaction (the app could remind the owner, and any record change also renews it), and
  Telegram controls `t.me` usernames' collection. A name is a convenience, never the only way to
  reach someone.
- Solana: a few RPC hosts; they can be blocked or can rate-limit, and they need no proof to lie.

### Linking a name in the app

"Link a name" in the profile:

1. The user types a name they own. The app resolves it and reads the current owner.
2. The app makes the binding: the identity signs (name, owner, now).
3. If the built-in wallet owns the name: a confirmation dialog shows the name, the owner wallet,
   what will be public (the privacy text above), and the cost (a message of ~0.05 TON to the NFT,
   most of it returned; the exact figure comes from an emulated fee). Only on the user's
   confirmation does the wallet send `change_dns_record` with the record. The wallet's `Transfer`
   takes only a text comment today; it needs a variant that carries a body cell.
4. If another wallet owns the name (Tonkeeper and others): the app shows a QR code and a
   `ton://transfer/<NFT address>?amount=…&bin=<base64url BOC>` link that wallet can sign. No TON
   Connect, because it relies on a bridge server.
5. The app resolves the name again and shows `verified`.

"Unlink" writes `change_dns_record` without a value for the same category.

### Buying a name in the app (later)

- `.ton`: a name is bought by an auction. Starting one sends at least the minimum price to the
  `.ton` collection with the name; the auction lasts a week, others can outbid, and the NFT goes to
  the winner. In the app this means: check availability with the resolver (`not-found`), show the
  minimum price and the week-long auction, and send the bid only after an explicit confirmation of
  the amount. The app must track the auction and say so if it was outbid (the bid comes back).
- `.t.me`: only through Fragment's site; the app links there.
- First version: link out to dns.ton.org and fragment.com, then use "Link a name". In-app bidding
  comes once linking works.

An EchoFog namespace (`alice.<project name>.ton` handed out by a subdomain contract) would give
cheap, short names and some revenue, but makes the project a naming authority. If it is ever done,
the contract must have no admin who can take names back.

## Prototype (`internal/names`)

- `name.go`: `Normalize` and `IsName` for `.ton` (with subdomains), `.t.me` and `.sol`; ASCII only.
- `binding.go`: the record format; `NewBinding` signs through a callback (the key manager keeps the
  private key); `ParseBinding`, `Verify`.
- `ton.go`: `dns_text` build/parse (chunked across cells); `TONRecordValue` for
  `change_dns_record`; `TONSource` over `tonutils-go/ton/dns` that returns the record and the NFT
  owner; a contract that throws (seen on a reserved `t.me` name) comes back as `invalid`.
- `resolver.go`: statuses, the in-memory cache with the offline fallback. `.sol` returns
  `ErrUnsupported`.
- Tests run offline with a fake source. `TestLiveTONLookup` reads mainnet only with
  `ECHOFOG_LIVE_TON=1` (extra names in `ECHOFOG_LIVE_TON_NAMES`). On 30 Oct 2026 it resolved a
  `.ton` name and several Fragment usernames with their owners through the public liteservers.

Not verified yet: writing the `echofog` category into a real `.ton` and `.t.me` item and reading
it back.

## Step plan

1. **Write test on mainnet.** Buy one cheap long `.ton` name and use one Fragment username; write a
   `fogname1:` record by hand (a script on `tonutils-go`) and read it back with the live test.
   Confirms custom categories on both item contracts.
2. **Answer hardening.** Two-liteserver agreement in `TONSource`, then parsing proved item state.
3. **Node API + desktop read path.** `ResolveName` in the node API and desktop bindings, sharing the
   wallet's liteserver client; `kind: 'name'` in `links.js`; the result card in `OpenLink.svelte`;
   "found by name" stored on the contact; the warning on the profile.
4. **Link a name.** A wallet transfer with a body; the confirmation dialog with the privacy text;
   the `ton://…&bin=` QR for external wallets; unlink.
5. **Persistent cache and proxy-aware lookups.** On-disk cache in the profile; a dialer hook for the
   liteclient so lookups follow the SOCKS/Tor setting.
6. **Buying `.ton` in the app.** Auction bid with explicit confirmation, auction tracking, renewal
   reminder.
7. **Optional: read-only `.sol`.** Records v2 `TXT` with the same `fogname1:` text, read from two
   RPC providers, labelled as less trustworthy than TON.

## Open questions

- Does the Fragment (`telemint`) item contract accept `change_dns_record` for an arbitrary
  category, as `.ton` items do? Step 1 answers it.
- Should a name be shown next to a contact's name in chats (like a verified handle), and how is it
  refreshed without background lookups?
- Should the record also carry an optional display name? It would be public and permanent; leaving
  it out for now.

---

## Кратко по-русски

**Вопрос:** можно ли находить людей в EchoFog по имени из криптосетей (`alice.ton`, `alice.sol`)
вместо длинного ключа или ссылки, без центрального сервера.

**Рекомендация:** начать с TON. Один резолвер читает и домены `.ton` (TON DNS), и юзернеймы
Telegram `alice.t.me` (NFT с Fragment, тот же интерфейс `dnsresolve`). Ядро уже ходит к
лайт-серверам TON через `tonutils-go` (кошелёк), в десктопе есть TON-кошелёк, и монетизация на TON.
Длинное имя `.ton` стоит от ~1 TON и ~0.015 TON в год на продление. Solana (`.sol`) — позже и только
на чтение: нет нашего кошелька, чтение через несколько HTTP RPC без доказательств. ENS (`.eth`) —
не сейчас: годовая плата плюс газ L1, переезд на ENSv2, нет лёгкого клиента на Go.

**Запись:** владелец имени кладёт в него текст `fogname1:…` (категория `sha256("echofog")`, тип
`dns_text`): ключ личности EchoFog, владелец имени на момент записи, время, и подпись ключом
личности поверх имени. Подпись не даёт привязать имя к чужому ключу без согласия и копировать
запись в другое имя; запись, оставшаяся после продажи имени, видна как «устаревшая». Новый владелец
имени может привязать его к своему ключу — от этого защищает то, что контакт всегда хранится по
ключу, а смена ключа за именем показывается предупреждением.

**Доверие:** `tonutils-go` проверяет доказательство состояния аккаунта, но результат get-метода
берёт у лайт-сервера на веру. План: сверять ответ двух лайт-серверов, затем разбирать доказанное
состояние NFT самим.

**Приватность:** имя публично и навсегда связывает кошелёк с личностью EchoFog. Если имя держит
встроенный кошелёк, которым платят за VPN, платежи становятся привязаны к личности — интерфейс
должен предупреждать и советовать отдельный кошелёк. Поиск идёт только по явному действию.

**Прототип:** `internal/names` — формат записи, подпись/проверка, `dns_text`, резолвер TON с
кешем и офлайн-ответом; тесты без сети, живой тест по `ECHOFOG_LIVE_TON=1` (30.10.2026 прочитал
`.ton` и юзернеймы Fragment с владельцами).

**Шаги:** проверка записи на mainnet → защита ответа → поиск в десктопе (Ctrl+L, карточка, пин) →
привязка имени из приложения с подтверждением → кеш на диске и прокси → покупка `.ton` в
приложении → `.sol` на чтение.
