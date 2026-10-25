import { writable } from 'svelte/store';
import { App } from '../api.js';

/**
 * TON network of the built-in wallet: 'mainnet', 'testnet', or '' until known. Read once from
 * GetTonWallet; screens that show money use it for the testnet notice (components/TestnetNotice.svelte).
 * `faucetHidden` is the notice's "got it" for this run of the app only: the faucet hint goes away,
 * the "test coins, not real money" line stays.
 */
export const tonNetwork = writable('');
export const faucetHidden = writable(false);

let loading = null;
export function loadTonNetwork() {
  if (!loading) {
    loading = App.GetTonWallet()
      .then((w) => tonNetwork.set(w?.network || ''))
      .catch(() => { loading = null; });
  }
  return loading;
}
