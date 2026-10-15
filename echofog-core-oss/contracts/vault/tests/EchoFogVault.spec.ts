import { Blockchain, SandboxContract, TreasuryContract } from '@ton/sandbox';
import { Address, beginCell, Cell, toNano, fromNano } from '@ton/core';
import { KeyPair, keyPairFromSeed } from '@ton/crypto';
import '@ton/test-utils';
import * as fs from 'fs';
import * as path from 'path';
import {
    EchoFogVault,
    MAINNET_GLOBAL_ID,
    TESTNET_GLOBAL_ID,
    signVoucher,
    vaultFor,
    voucherCell,
    VOUCHER_MAGIC,
} from '../wrappers/EchoFogVault';

const ERR = {
    BAD_VOUCHER: 1000,
    WRONG_NETWORK: 1001,
    WRONG_VAULT: 1002,
    BAD_SIGNATURE: 1003,
    NOTHING_TO_CLAIM: 1004,
    EMPTY_DEPOSIT: 1005,
    NOT_OWNER: 1006,
    NO_WITHDRAW: 1007,
    COOLDOWN: 1008,
    LOW_GAS: 1009,
    TOO_SMALL: 1010,
};

const STORAGE_RESERVE = toNano('0.005');
const DEPOSIT_GAS = toNano('0.003');
const CLAIM_GAS = toNano('0.02'); // what the node attaches; the unused part comes back

// a deposit is credited minus at most a little gas
function expectAbout(got: bigint, want: bigint) {
    expect(got).toBeLessThanOrEqual(want);
    expect(got).toBeGreaterThan(want - toNano('0.001'));
}

// The sandbox runs with the mainnet config (global id -239).
const GLOBAL_ID = MAINNET_GLOBAL_ID;

// Shared with the Go tests: the same seed, addresses and amount must give the same bytes there.
const FIXTURE = path.join(__dirname, '../../../internal/wallet/tonwallet/testdata/voucher_fixture.json');

describe('EchoFogVault', () => {
    let blockchain: Blockchain;
    let owner: SandboxContract<TreasuryContract>;
    let node: SandboxContract<TreasuryContract>;
    let node2: SandboxContract<TreasuryContract>;
    let stranger: SandboxContract<TreasuryContract>;
    let keys: KeyPair;
    let vault: SandboxContract<EchoFogVault>;

    async function deposit(amount: bigint, from = owner) {
        return vault.send(from.getSender(), { value: amount }, { $$type: 'Deposit', queryId: 0n });
    }

    async function claim(cumulative: bigint, opts: { by?: SandboxContract<TreasuryContract>; to?: Address; key?: Buffer; globalId?: number; vaultAddr?: Address; value?: bigint } = {}) {
        const by = opts.by ?? node;
        const { cell, signature } = signVoucher(
            {
                globalId: opts.globalId ?? GLOBAL_ID,
                vault: opts.vaultAddr ?? vault.address,
                node: opts.to ?? node.address,
                cumulative,
            },
            opts.key ?? keys.secretKey,
        );
        return vault.send(
            by.getSender(),
            { value: opts.value ?? CLAIM_GAS },
            { $$type: 'Claim', queryId: 0n, voucher: cell, signature: signature },
        );
    }

    async function state() {
        return vault.getState();
    }

    beforeEach(async () => {
        blockchain = await Blockchain.create();
        owner = await blockchain.treasury('owner');
        node = await blockchain.treasury('node');
        node2 = await blockchain.treasury('node2');
        stranger = await blockchain.treasury('stranger');
        keys = keyPairFromSeed(Buffer.alloc(32, 7));
        vault = blockchain.openContract(await vaultFor(owner.address, keys.publicKey));
    });

    it('is deployed by the first deposit and credits it minus the storage reserve', async () => {
        const res = await deposit(toNano('1'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, deploy: true, success: true });
        const s = await state();
        expect(s.owner.equals(owner.address)).toBe(true);
        expect(s.deposit).toBe(toNano('1') - STORAGE_RESERVE - DEPOSIT_GAS);
        expect(s.withdrawAt).toBe(0n);
        expect(await vault.getVersion()).toBe(1n);

        // a later deposit is credited minus the gas of the transaction before it: each deposit
        // pays its own network fee, a fraction of a cent
        const first = (await state()).deposit;
        await deposit(toNano('2'));
        expectAbout((await state()).deposit - first, toNano('2'));
    });

    it('accepts plain transfers and text comments as deposits', async () => {
        await deposit(toNano('1'));
        const before = (await state()).deposit;
        await stranger.send({ to: vault.address, value: toNano('0.5') });
        await stranger.send({ to: vault.address, value: toNano('0.5'), body: beginCell().storeUint(0, 32).storeStringTail('top up').endCell() });
        expectAbout((await state()).deposit - before, toNano('1'));
    });

    it('refuses a deposit too small to cover the reserve', async () => {
        const res = await deposit(toNano('0.006'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.TOO_SMALL });
    });

    it('pays the delta of a cumulative voucher once', async () => {
        await deposit(toNano('1'));
        const dep = (await state()).deposit;

        let res = await claim(toNano('0.1'));
        expect(res.transactions).toHaveTransaction({ from: vault.address, to: node.address, success: true });
        expect(await vault.getClaimedBy(node.address)).toBe(toNano('0.1'));
        expect((await state()).deposit).toBe(dep - toNano('0.1'));

        // the same voucher again pays nothing
        res = await claim(toNano('0.1'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.NOTHING_TO_CLAIM });

        // an older one neither
        res = await claim(toNano('0.05'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.NOTHING_TO_CLAIM });

        // a newer one pays only the difference
        res = await claim(toNano('0.25'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: true });
        expect(await vault.getClaimedBy(node.address)).toBe(toNano('0.25'));
        expect((await state()).deposit).toBe(dep - toNano('0.25'));
    });

    it('sends the payout plus unused gas to the node', async () => {
        await deposit(toNano('1'));
        const before = await node.getBalance();
        const res = await claim(toNano('0.2'));
        const after = await node.getBalance();
        const cost = toNano('0.2') - (after - before);
        // what one claim really costs the node: wallet send + vault compute + payout forwarding
        console.log(`one claim costs the node ${fromNano(cost)} TON`);
        expect(cost).toBeGreaterThan(0n);
        expect(cost).toBeLessThan(toNano('0.01'));
        const vaultTx = res.transactions.find((t) => t.inMessage?.info.dest?.toString() === vault.address.toString());
        expect(vaultTx).toBeDefined();
    });

    it('keeps separate totals per node', async () => {
        await deposit(toNano('1'));
        await claim(toNano('0.1'));
        const res = await claim(toNano('0.1'), { by: node2, to: node2.address });
        expect(res.transactions).toHaveTransaction({ from: vault.address, to: node2.address, success: true });
        expect(await vault.getClaimedBy(node.address)).toBe(toNano('0.1'));
        expect(await vault.getClaimedBy(node2.address)).toBe(toNano('0.1'));
    });

    it('lets anyone submit a claim, but pays only the node in the voucher', async () => {
        await deposit(toNano('1'));
        const res = await claim(toNano('0.1'), { by: stranger });
        expect(res.transactions).toHaveTransaction({ from: vault.address, to: node.address, success: true });
        expect(res.transactions).not.toHaveTransaction({ from: vault.address, to: stranger.address });
    });

    it('pays partially when the deposit is short, and the rest after a top-up', async () => {
        await deposit(toNano('0.5'));
        const dep = (await state()).deposit;

        let res = await claim(toNano('0.8'));
        expect(res.transactions).toHaveTransaction({ from: vault.address, to: node.address, success: true });
        expect(await vault.getClaimedBy(node.address)).toBe(dep);
        expect((await state()).deposit).toBe(0n);

        // nothing left: the same voucher fails until the owner tops up
        res = await claim(toNano('0.8'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.EMPTY_DEPOSIT });

        await deposit(toNano('1'));
        res = await claim(toNano('0.8'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: true });
        expect(await vault.getClaimedBy(node.address)).toBe(toNano('0.8'));
    });

    it('rejects vouchers that are forged, foreign or malformed', async () => {
        await deposit(toNano('1'));
        const other = keyPairFromSeed(Buffer.alloc(32, 9));

        let res = await claim(toNano('0.1'), { key: other.secretKey });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.BAD_SIGNATURE });

        res = await claim(toNano('0.1'), { globalId: TESTNET_GLOBAL_ID });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.WRONG_NETWORK });

        res = await claim(toNano('0.1'), { vaultAddr: stranger.address });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.WRONG_VAULT });

        res = await claim(toNano('0.1'), { value: toNano('0.005') });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.LOW_GAS });

        // a signed cell that is not a voucher (wrong magic)
        const junk = beginCell().storeUint(VOUCHER_MAGIC + 1, 32).endCell();
        const { sign } = await import('@ton/crypto');
        res = await vault.send(node.getSender(), { value: CLAIM_GAS }, {
            $$type: 'Claim',
            queryId: 0n,
            voucher: junk,
            signature: sign(junk.hash(), keys.secretKey),
        });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.BAD_VOUCHER });

        // a voucher with a field changed after signing
        const signed = signVoucher({ globalId: GLOBAL_ID, vault: vault.address, node: node.address, cumulative: toNano('0.1') }, keys.secretKey);
        const tampered = voucherCell({ globalId: GLOBAL_ID, vault: vault.address, node: node.address, cumulative: toNano('0.9') });
        res = await vault.send(node.getSender(), { value: CLAIM_GAS }, {
            $$type: 'Claim',
            queryId: 0n,
            voucher: tampered,
            signature: signed.signature,
        });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.BAD_SIGNATURE });

        expect(await vault.getClaimedBy(node.address)).toBe(0n);
    });

    it('withdraws in two steps, with claims still allowed during the cooldown', async () => {
        await deposit(toNano('1'));

        let res = await vault.send(stranger.getSender(), { value: toNano('0.05') }, { $$type: 'RequestWithdraw', queryId: 0n });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.NOT_OWNER });

        res = await vault.send(owner.getSender(), { value: toNano('0.05') }, { $$type: 'ExecuteWithdraw', queryId: 0n });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.NO_WITHDRAW });

        const t0 = Math.floor(Date.now() / 1000);
        blockchain.now = t0;
        res = await vault.send(owner.getSender(), { value: toNano('0.05') }, { $$type: 'RequestWithdraw', queryId: 0n });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: true });
        expect((await state()).withdrawAt).toBe(BigInt(t0 + 86400));

        blockchain.now = t0 + 3600;
        res = await vault.send(owner.getSender(), { value: toNano('0.05') }, { $$type: 'ExecuteWithdraw', queryId: 0n });
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.COOLDOWN });

        // a node cashes what it was already signed for
        res = await claim(toNano('0.3'));
        expect(res.transactions).toHaveTransaction({ from: vault.address, to: node.address, success: true });
        const left = (await state()).deposit;

        blockchain.now = t0 + 86400;
        const before = await owner.getBalance();
        res = await vault.send(owner.getSender(), { value: toNano('0.05') }, { $$type: 'ExecuteWithdraw', queryId: 0n });
        expect(res.transactions).toHaveTransaction({ from: vault.address, to: owner.address, success: true });
        const got = (await owner.getBalance()) - before;
        expect(got).toBeGreaterThan(left - toNano('0.01'));
        const s = await state();
        expect(s.deposit).toBe(0n);
        expect(s.withdrawAt).toBe(0n);

        // the contract is still there and still remembers what was paid
        expect(await vault.getClaimedBy(node.address)).toBe(toNano('0.3'));
        res = await claim(toNano('0.3'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.NOTHING_TO_CLAIM });

        // after a new deposit an old voucher still cannot be replayed
        await deposit(toNano('1'));
        res = await claim(toNano('0.3'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: false, exitCode: ERR.NOTHING_TO_CLAIM });
        res = await claim(toNano('0.4'));
        expect(res.transactions).toHaveTransaction({ to: vault.address, success: true });
        expect(await vault.getClaimedBy(node.address)).toBe(toNano('0.4'));
    });

    it('can cancel a pending withdrawal', async () => {
        await deposit(toNano('1'));
        await vault.send(owner.getSender(), { value: toNano('0.05') }, { $$type: 'RequestWithdraw', queryId: 0n });
        expect((await state()).withdrawAt).not.toBe(0n);
        await vault.send(owner.getSender(), { value: toNano('0.05') }, { $$type: 'CancelWithdraw', queryId: 0n });
        expect((await state()).withdrawAt).toBe(0n);
    });

    it('measures a batch of claims to many user vaults sent in one go', async () => {
        const n = 20;
        const vaults: SandboxContract<EchoFogVault>[] = [];
        const signed: { cell: Cell; signature: Buffer }[] = [];
        for (let i = 0; i < n; i++) {
            const user = await blockchain.treasury('user' + i);
            const k = keyPairFromSeed(Buffer.alloc(32, 100 + i));
            const v = blockchain.openContract(await vaultFor(user.address, k.publicKey));
            await v.send(user.getSender(), { value: toNano('1') }, { $$type: 'Deposit', queryId: 0n });
            vaults.push(v);
            signed.push(signVoucher({ globalId: GLOBAL_ID, vault: v.address, node: node.address, cumulative: toNano('0.05') }, k.secretKey));
        }
        const before = await node.getBalance();
        for (let i = 0; i < n; i++) {
            await vaults[i].send(node.getSender(), { value: CLAIM_GAS }, {
                $$type: 'Claim',
                queryId: BigInt(i),
                voucher: signed[i].cell,
                signature: signed[i].signature,
            });
        }
        const earned = (await node.getBalance()) - before;
        const cost = toNano('0.05') * BigInt(n) - earned;
        console.log(`${n} claims of 0.05 TON: node got ${fromNano(earned)} TON, gas ${fromNano(cost)} TON (${fromNano(cost / BigInt(n))} each)`);
        expect(earned).toBeGreaterThan(toNano('0.04') * BigInt(n));
    });

    it('accepts a voucher built from the shared Go fixture', async () => {
        const fx = JSON.parse(fs.readFileSync(FIXTURE, 'utf8'));
        const fxKeys = keyPairFromSeed(Buffer.from(fx.seed_hex, 'hex'));
        expect(fxKeys.publicKey.toString('hex')).toBe(fx.pubkey_hex);

        const fxOwner = Address.parse(fx.owner);
        const fxNode = Address.parse(fx.node);
        const fxVault = await vaultFor(fxOwner, fxKeys.publicKey);
        expect(fxVault.address.toRawString()).toBe(fx.vault_raw);
        expect(fxVault.init!.code.hash().toString('hex')).toBe(fx.code_hash_hex);

        const v = { globalId: fx.global_id, vault: fxVault.address, node: fxNode, cumulative: BigInt(fx.cumulative) };
        const { cell, signature } = signVoucher(v, fxKeys.secretKey);
        expect(cell.toBoc().toString('hex')).toBe(fx.voucher_boc_hex);
        expect(cell.hash().toString('hex')).toBe(fx.voucher_hash_hex);
        expect(signature.toString('hex')).toBe(fx.signature_hex);

        // and the contract accepts exactly those bytes
        const deployed = blockchain.openContract(fxVault);
        await deployed.send(owner.getSender(), { value: toNano('1') }, { $$type: 'Deposit', queryId: 0n });
        const res = await deployed.send(node.getSender(), { value: CLAIM_GAS }, {
            $$type: 'Claim',
            queryId: 0n,
            voucher: Cell.fromBoc(Buffer.from(fx.voucher_boc_hex, 'hex'))[0],
            signature: Buffer.from(fx.signature_hex, 'hex'),
        });
        expect(res.transactions).toHaveTransaction({ from: deployed.address, to: fxNode, value: (x) => x! > 0n });
        expect(await deployed.getClaimedBy(fxNode)).toBe(BigInt(fx.cumulative));
    });
});
