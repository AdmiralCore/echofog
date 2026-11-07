Русская версия: [domestic-entry.ru.md](domestic-entry.ru.md)

# Home entries and covers for Russia and Iran

Data as of: 2026-11-07

This is the operational document for people who run a **home entry**: a relay node inside the country
of the users, which they reach first, and which then carries their traffic out. It is written for
operators and for the curator of the rules file. No address in this document belongs to a real person
or a real network; the examples use documentation ranges (`203.0.113.0/24`, `198.51.100.0/24`).

## 1. Why a home entry

- In some mobile networks of Russia a TCP connection to a foreign data centre stalls after roughly
  16 to 20 KB ("freeze"), and during shutdowns only addresses on allow-lists work. A connection to an
  address inside the country usually keeps working.
- A client that cannot reach the world directly therefore goes to a node inside the country first.
  That node is the **home entry** (T6.4 chooses it as the first hop when the network needs it).
- The same idea works with nodes in neighbouring countries (section 8).

## 2. What a home entry is, and is not

- It is a relay (and mailbox, DHT) with a separate invitation port. It is created by the installer
  preset `home-entry`: `relay`, no exit, the `domestic-only` flag off.
- It is **not** an exit. Traffic to the internet leaves through exits outside the country; the home
  entry only relays encrypted sessions.
- The preset is listed openly in the installer next to the others, with the warning of section 5.

## 3. Where to host it

- The address must be inside the country and, for **mobile** networks that run on allow-lists, inside
  the verified ranges (`allowed_cidrs`) of the signed rules file. An address outside those ranges is
  good for **wired** networks only; the installer says so (the note "wired networks only").
- Which ranges and clouds are allowed is measured, not guessed: it comes from the field probe T15.2a
  and the research T15.2 and from public sources, each entry with its date. **There is no measured
  list yet** (the probe waits for a tester with Russian SIM cards): until then the policy assumes the
  worst case and the first rules file has no verified ranges. Do not copy ranges from anywhere else
  into the file.
- Prefer a provider that is allowed to serve the country's mobile users in the measurement, a fixed
  IPv4 address, and UDP not filtered.

## 4. Which cover to choose

- The cover is a real site the node imitates. Use a domain you control, a real site of your own with
  real content (there is no shared template), and a certificate that you renew (`v2 doctor` watches the
  expiry).
- A free dynamic DNS name works but is more conspicuous and risks a block of the whole service; the
  installer warns about it. Without a domain the node serves only its own people and has no public card.
- Do not imitate a well-known service you do not run.

## 5. What the operator sees, and the risks

This section is not legal advice.

- **Sees:** the network addresses of the people who connect, when and how much. It does not see the
  content of messages or calls (end-to-end encryption), and it does not see where an exit sends the traffic.
- **Risks:** a request to hand over data or logs; local rules that may restrict running such a node; a
  block of the address.
- **Lower the risk:** a separate server with no personal data on it; the exit switched off (the preset
  does this); minimal logs; an anonymous contact only for complaints; read the rules of the country and of
  the hosting provider before the start.

## 6. Install the preset

```
ECHOFOG_RELEASE_URL=https://example.org/releases/v2 sh install.sh --preset home-entry --advertise 203.0.113.10 --lang en
```

Then run `echofog-node v2 doctor --data /var/lib/echofog` and `v2 status`. The full steps are in
[deploy-vps-v2.md](deploy-vps-v2.md). The check "the node is chosen as the first hop by T6.4" needs a test
server and a client in the country: the owner runs it.

## 7. The rules file

- **Curator and key holder: the owner.** The rules key is separate from the release keys and stays
  **offline**. It is made with `efv2-rules keygen`, on offline media, never inside the repository.
- **Update monthly.** One whole signed file each time (no partial updates, no telemetry): the node never
  reports what it uses. `seq` rises with every file; `valid_until` is 30 days after signing.
- **Expiry never loosens anything.** After `valid_until` the verified ranges (`allowed_cidrs`) stop
  counting, while the restrictions of the file (the TCP cap, `udp_denied`) stay in force.
- **Signing:** `efv2-rules sign --key rules.key --body body.json --prev last.bin --out rules.bin` sets
  `seq` (previous plus one) and `valid_until` itself and refuses what the node would refuse;
  `efv2-rules verify --pub PUBHEX --in rules.bin` checks a file. The first file is built from
  `deploy/rules/first-rules.example.json`: neighbours of the table, no verified ranges yet.
- **Mirror:** an HTTPS mirror serves the file whole and does not log the country or address of the
  client. For nginx: `access_log off;` for the file's location and no log of the client address anywhere
  in the chain.
- A verified range is added only after the curator has checked the entry from a SIM in the country (T15.2a).

## 8. Neighbouring countries

Nodes in neighbouring countries (for example KZ, BY, AM, AZ, GE) can serve as an entry or exit for
users in Russia. Until the measurement says otherwise they are treated like any foreign country: the
rules file lists them as neighbours (data for the report), and the table of T6.4 does not trust them more
than Europe. The measurement of the field probe decides their class.

## 9. What is not verified yet

- No measured allow-list ranges and no freeze threshold from real Russian mobile networks (T15.2a).
- No live installation of the preset on a test VPS and no check that T6.4 picks it as the first hop.
- The signing tool and the mirror are checked on test keys only; the owner's offline key does not exist yet.
