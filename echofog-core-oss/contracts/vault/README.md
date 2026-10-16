# EchoFogVault contract

One user's TON deposit, which paid proxy nodes collect from with signed cumulative vouchers. How
it works for users and node operators: [docs/vault-payments.md](../../../docs/vault-payments.md).

- `contracts/echofog_vault.tact` is the contract (Tact 1.6.13, pinned).
- `tests/EchoFogVault.spec.ts` holds the sandbox tests: deposits, claims, partial payout,
  forged/foreign vouchers, the two-step withdrawal, gas measurements, and the shared Go fixture.
- `wrappers/EchoFogVault.ts` is the voucher format in TypeScript (it mirrors `tonwallet/vault.go`).
- `scripts/deployVault.ts` creates one vault by hand.
- `scripts/makeFixture.ts` regenerates the fixture shared with the Go tests.

## Build and test

```bash
npm ci
npm run build    # compiles and copies the code into ../../internal/wallet/tonwallet/vault_code.boc
npm test
```

`npm run build` must leave `vault_code.boc` unchanged unless you changed the contract: CI checks
that the Go app embeds exactly this build. If you change the contract on purpose, rebuild, run
`npm run fixture` and commit the new `vault_code.boc` and `voucher_fixture.json` together. A
changed contract gives every user a new vault address. Existing deposits stay in the old vaults
and must be withdrawn from there.

## Deploying

No one deploys "the" vault. Each user's vault is created by its first deposit, and the EchoFog app
does that by itself. What needs publishing is the **code hash**, so anyone can check that a vault
runs this code:

```bash
npm run build
node -e "const {Cell}=require('@ton/core');console.log(Cell.fromBoc(require('fs').readFileSync('build/EchoFogVault/tact_EchoFogVault.code.boc'))[0].hash().toString('hex'))"
```

To try a vault by hand, go to **testnet first**:

1. Get a testnet wallet (Tonkeeper in testnet mode, or `--mnemonic` with a testnet seed) and
   testnet TON from the [@testgiver_ton_bot](https://t.me/testgiver_ton_bot).
2. Run `npx blueprint run deployVault --testnet --tonconnect` (or `--mnemonic`, with `WALLET_MNEMONIC`
   and `WALLET_VERSION=v5r1` in the environment).
3. Enter the ed25519 public key that will sign vouchers (64 hex characters) and the first deposit
   (at least 0.1 TON). The connected wallet becomes the vault owner.
4. The script prints the vault address and its state. Check it in a testnet explorer
   (`testnet.tonviewer.com`).

In the app, set `ECHOFOG_TON_NETWORK=testnet` (or `"ton": {"network": "testnet"}` in the config) to
use testnet end to end.

**Mainnet**: the same with `--mainnet`, only after an independent security audit of the contract by a specialist firm. The code
is immutable and has no admin key, so a mistake cannot be fixed later; it can only be replaced by
a new version with new vault addresses.

## Exit codes

| Code | Meaning |
|---|---|
| 1000 | not a voucher cell |
| 1001 | voucher for the other TON network |
| 1002 | voucher for another vault |
| 1003 | bad signature |
| 1004 | nothing to claim (this or a newer voucher was already paid) |
| 1005 | the deposit is empty |
| 1006 | not the owner |
| 1007 | no withdrawal requested |
| 1008 | the 24 hours are not over |
| 1009 | the claim brought less than 0.01 TON for gas |
| 1010 | the deposit does not cover the storage reserve |

## A note on Tact

Tact is marked deprecated in favour of Tolk, and Blueprint 0.47 dropped it. Here that changes
nothing: the deployed bytecode does not depend on the compiler being maintained, and both the
compiler (1.6.13) and Blueprint (0.46.0) are pinned. A Tolk port would be a new contract version
with new vault addresses.
