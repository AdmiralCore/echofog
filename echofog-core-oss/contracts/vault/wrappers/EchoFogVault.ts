// Re-exports the Tact-generated bindings and adds the voucher format shared with the Go node
// (echofog-core-oss/internal/wallet/tonwallet/voucher.go). Keep the two in step.
import { Address, beginCell, Cell, Dictionary } from '@ton/core';
import { sign } from '@ton/crypto';
import { EchoFogVault } from '../build/EchoFogVault/tact_EchoFogVault';

export * from '../build/EchoFogVault/tact_EchoFogVault';

export const VOUCHER_MAGIC = 0x45465631; // "EFV1"
export const MAINNET_GLOBAL_ID = -239;
export const TESTNET_GLOBAL_ID = -3;

export type Voucher = {
    globalId: number;
    vault: Address;
    node: Address;
    cumulative: bigint; // nanoton
};

export function voucherCell(v: Voucher): Cell {
    return beginCell()
        .storeUint(VOUCHER_MAGIC, 32)
        .storeInt(v.globalId, 32)
        .storeAddress(v.vault)
        .storeAddress(v.node)
        .storeCoins(v.cumulative)
        .endCell();
}

/** Signs the voucher cell hash with a 64-byte ed25519 secret key (as from @ton/crypto). */
export function signVoucher(v: Voucher, secretKey: Buffer): { cell: Cell; signature: Buffer } {
    const cell = voucherCell(v);
    return { cell, signature: sign(cell.hash(), secretKey) };
}

/** The user's vault in its initial state; its address is the one every wallet and node computes. */
export function vaultFor(owner: Address, pubKey: Buffer): Promise<EchoFogVault> {
    return EchoFogVault.fromInit(owner, BigInt('0x' + pubKey.toString('hex')), 0n, 0n, Dictionary.empty());
}
