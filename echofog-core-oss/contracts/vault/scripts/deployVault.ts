// Creates (deploys) one user's vault with its first deposit, and shows its state.
//
// There is no "the" EchoFogVault to deploy: every user has their own, and the EchoFog app creates
// it by itself with the first deposit. This script is for trying the contract by hand, on testnet
// first:
//
//   npx blueprint run deployVault --testnet --tonconnect      (or --mnemonic, see README)
//
// It asks for the ed25519 public key that will sign vouchers (64 hex characters; in the app it is
// the wallet key) and for the first deposit. The connected wallet becomes the vault owner: the only
// address that can withdraw.
import { toNano, fromNano } from '@ton/core';
import { NetworkProvider } from '@ton/blueprint';
import { vaultFor } from '../wrappers/EchoFogVault';

export async function run(provider: NetworkProvider) {
    const ui = provider.ui();
    const owner = provider.sender().address;
    if (!owner) {
        throw new Error('connect a wallet first');
    }
    const keyHex = (await ui.input('Voucher public key (64 hex characters)')).trim().toLowerCase();
    if (!/^[0-9a-f]{64}$/.test(keyHex)) {
        throw new Error('the key must be 32 bytes in hex');
    }
    const amount = toNano((await ui.input('First deposit in TON (at least 0.1)')).trim());
    if (amount < toNano('0.1')) {
        throw new Error('the first deposit must be at least 0.1 TON: it also creates the vault');
    }

    const vault = provider.open(await vaultFor(owner, Buffer.from(keyHex, 'hex')));
    ui.write(`Vault address: ${vault.address.toString({ testOnly: provider.network() !== 'mainnet' })}`);

    await vault.send(provider.sender(), { value: amount, bounce: false }, { $$type: 'Deposit', queryId: 0n });
    await provider.waitForDeploy(vault.address);

    const s = await vault.getState();
    ui.write(`Deposit: ${fromNano(s.deposit)} TON, withdrawal pending: ${s.withdrawAt !== 0n}`);
    ui.write(`Code hash: ${vault.init!.code.hash().toString('hex')} (must match the one in the EchoFog release)`);
}
