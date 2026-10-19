# Paying for traffic from a TON vault

*How EchoFog users pay proxy/VPN nodes, in plain words. A Russian version is in
[vault-payments.ru.md](vault-payments.ru.md).*

> **Status: testnet.** The contract and the code around it are new and have **not** had an
> external audit. Use testnet TON until the mainnet release is announced.

## In one paragraph

You put some TON into **your own small contract**, called a *vault*. While you use a paid node,
your app keeps writing it small signed notes, called *vouchers*: "this node may take up to X TON
from my vault, in total". Nothing goes on the blockchain while you browse, so there is no network
fee per megabyte. Every few hours the node takes what you owe it from the vault, in one go. You
can take back what is left at any time; it arrives 24 hours after you ask. There is no
commission: the project takes nothing from these payments.

## Who is who

| | What it is |
|---|---|
| **You (the user)** | Anyone using a paid node. Calls, chat and free nodes need none of this. |
| **The node** | Someone who shares their Internet connection as a VPN exit and sets a price per GB. |
| **The vault** | A small TON contract that belongs to you. Only you can withdraw from it. A node can only take what you signed for. |

Every user has their own vault. Its address is calculated from the contract code and your wallet,
so it is the same every time and anyone can check it. Nobody runs the vault, there is no "admin" key,
and the code can never be changed after it is published.

## The life of a payment

1. **Deposit.** In the wallet you choose an amount and press *Deposit*. The first deposit also
   creates your vault; that costs about 0.008 TON once, which stays in the contract to pay its
   storage. Later deposits cost a tiny network fee.
2. **Browsing.** Before you use the next 64 MB, your app signs a voucher that raises the total by
   the price of 64 MB. At $0.05 per GB that is about a third of a cent. The node checks the
   signature, and every minute or so it checks on the blockchain that your vault really has the
   money. Then it lets the traffic through. So you never owe more than you have, and you never pay
   more than 64 MB ahead.
3. **The node takes its money.** The node keeps only your latest voucher, because each one
   includes all the earlier ones. Every 6 hours, or sooner once enough has built up, it sends the
   vouchers to the vaults. Each vault pays the difference between the new total and what that node
   already took. An old or repeated voucher pays nothing.
4. **Withdrawing.** Press *Withdraw*. For the next 24 hours nodes can still collect what you
   already signed for, but they stop selling you new traffic from this deposit. After 24 hours
   press *Finish withdrawal* and the rest comes back to your wallet. Changed your mind? Press
   *Cancel* during the 24 hours.

Why 24 hours: without that wait you could use a node, sign vouchers, and then empty the vault
before the node collects. The wait is the node's guarantee that what you signed for is still there.

## What it costs

| What | Who pays | How much |
|---|---|---|
| Traffic | you | the node's price, nothing on top |
| Commission for the project | nobody | **none** |
| Creating your vault (first deposit) | you | about 0.008 TON, once |
| Each later deposit | you | a fraction of a cent of network fee |
| Withdraw request / cancel / finish | you | a fraction of a cent each (0.02 TON is attached, the rest comes back) |
| Each voucher while browsing | nobody | free: it never touches the blockchain |
| Collecting a voucher | the node | about 0.0012 TON, measured in the contract tests |

You can support the project voluntarily: the share is set in the wallet (0–20%, off with one
click). It is counted on what you actually spend, not on what you deposit, and it is never a
condition of service.

## Who risks what

Every payment system has risks. These are ours:

- **You risk at most one chunk (64 MB)**, paid ahead and possibly not served if the node goes away.
- **A node risks what it has not collected yet.** A modified app could sign the same deposit to
  many nodes at once. The first to collect gets paid. Collection is first come, first served,
  and a voucher that could only be paid in part pays the rest after you top up. A node limits
  this by collecting from any user who owes it more than 0.05 TON right away, without waiting for
  the 6-hour round.
- **A node that is offline for more than 24 hours** during a user's withdrawal can miss what that
  user owed it.
- **The contract has not been audited yet.** It is small (about 200 lines) and fully tested, but
  that does not replace an independent security audit by a specialist firm. That is why testnet
  comes first.
- **Privacy.** Like any payment on TON, a deposit and the node's collection are public: someone
  watching the blockchain can see that your wallet paid a node's wallet. They cannot see what you
  did through it. If this matters to you, use a fresh wallet funded through an exchange.

## You don't always need a vault

- **Your own node.** Run a node on your own server and set its price to 0, or put yourself and
  your friends on its free list. You pay nothing and need no vault and no TON at all.
- **Free nodes** work without any of this.
- **Paying by direct transfer** still works: a node that does not take vouchers, or paying in
  USDT, uses the older way, where each chunk is a TON or USDT transfer to the node. When both you
  and the node can use the vault, the app uses the vault for TON.

## Why one contract per user, and not one for everyone

A single shared contract that remembered everyone's payments would run into TON's limit on how
much one contract can store, at about 20–30 thousand user–node pairs. Its storage fee would also
grow and come out of everyone's deposits. A small contract per user costs about 0.008 TON once,
stays tiny, and has no owner and nothing to upgrade. USDT on TON works the same way: every holder
has their own small contract.

## Why a node can trust a voucher

A voucher is a few fields signed with your wallet key:

- a fixed tag `EFV1`, so a voucher can never be mistaken for a wallet transfer signed with the same
  key;
- the **network** (mainnet or testnet), so a testnet voucher is useless on mainnet;
- **your vault's address**, so a voucher cannot be used against someone else's vault;
- **the node's address**, the only place the money can go;
- **the total** this node may take, ever.

The contract checks all of this and the signature before it pays. The node derives your vault's
address itself from the published code, so it cannot be shown a fake contract that lies about its
balance.

## For node operators

### Put a little TON on the node's wallet first

A priced exit gets paid in two ways, and one of them needs TON on the node's own wallet:

- **Direct payments** (a client sends USDT or TON to the node's wallet) arrive by themselves.
- **Payments from a client's deposit** (the TON vault, the usual way in the app) do not move until
  the node collects them. Collecting is a transaction to the client's vault, and like every TON
  transaction it costs a fee that **the node's wallet pays**: about 0.015 TON per voucher, with
  0.015 TON kept back for the wallet's own fee.

A new node's wallet is empty, so it cannot collect anything, even though clients have already paid
(their vouchers are signed and waiting). Put about **0.1–0.2 TON** on it; that covers many
collections.

1. Find the wallet address:

   ```bash
   docker compose -f deploy/docker-compose.yml exec node echofog-node status
   ```

   The line `Wallet (testnet): <address>` shows the address, the next one the balance. It is also in every stats line (`wallet: 0 TON`).
2. Send TON to it. On testnet (the default) take free test TON from the Telegram bot
   **@testgiver_ton_bot**.
3. Within five minutes the node collects what clients owe from their deposits (`cashed vouchers` in
   the log).

**What you see while the wallet is empty:** the stats line ends with
`vouchers: 0.74 TON owed, 0 TON cashed (the node wallet has no TON to pay the fee for cashing
vouchers)`, the log says once an hour how much is waiting and the address to top up, and the app's
sharing card shows the same with a button to copy the address.

**When the node collects:** it checks every 5 minutes. A client who owes 0.05 TON or more, a total
of 0.2 TON or more, or a client withdrawing their deposit is collected at once; otherwise every
6 hours. Debts under 0.02 TON wait for the next round, because the fee would cost more than they
bring.

### The rest

- Turn on payments as before: set a price. If the TON price is known, your node also quotes the
  price in TON, re-checked every 10 minutes from your USD price. Clients check that quote against
  their own rate and refuse it if it is more than 5% higher.
- Keep about **0.1 TON** in the node's wallet. Collecting attaches 0.015 TON per voucher for gas,
  and the vault sends back what is not used.
- Collection happens by itself: every 6 hours, as soon as the total reaches 0.2 TON, or at once
  for a user who owes more than 0.05 TON or is withdrawing. Users who owe less than 0.02 TON wait
  for the next round; it would cost more gas than it brings.
- The node keeps vouchers in `ton/exit-vouchers.json` next to its database. It keeps collecting
  them after you stop sharing. **Do not delete this file while it holds unpaid vouchers: they are
  your money.**

## For developers

| Where | What |
|---|---|
| [`echofog-core-oss/contracts/vault/`](../echofog-core-oss/contracts/vault/) | the Tact contract, its tests and deploy scripts ([README](../echofog-core-oss/contracts/vault/README.md)) |
| `echofog-core-oss/internal/wallet/tonwallet/vault.go` | voucher cells, signing, the vault address, deposits, withdrawals, batch claims |
| `echofog-core-oss/internal/network/exit_gateway/vouchers.go` | the node side: checking vouchers, metering traffic, the claim worker |
| `echofog-core-oss/pkg/api/node/vault.go` | the app side: when to pay by voucher, the nanoton price, the API |

The Go code embeds the compiled contract (`vault_code.boc`). CI rebuilds the contract from source
and fails if the two differ. A shared fixture (`tonwallet/testdata/voucher_fixture.json`) is
checked by both the contract tests and the Go tests, so Go and the contract agree byte for byte
on the vault address, the voucher and its signature.

Voucher cell: `uint32 0x45465631 | int32 globalId | MsgAddress vault | MsgAddress node | Coins
cumulative`. The signature is ed25519 over the cell hash, as TVM `CHKSIGNU` checks it.
