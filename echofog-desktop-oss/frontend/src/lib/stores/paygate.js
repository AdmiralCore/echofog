import { writable } from 'svelte/store';
import { App } from '../api.js';
import { tr } from '../i18n/index.js';

/**
 * Payments that ask the app password ("remember on this computer" with "ask the password before
 * payments"). Wrap every call that sends money:
 *
 *   hash = await pay(() => App.SendTon(to, amount, comment));
 *
 * When this session asks the password, the PaymentPassword window takes it (App.AuthorizePayment)
 * before the call; the Go side refuses the payment without it either way.
 */
export const payPrompt = writable(null); // { resolve } while the window is open

export async function pay(send) {
  let gate = false;
  try { gate = (await App.GetRememberedUnlock())?.paymentGate; } catch (_) { /* the Go side still checks */ }
  if (gate && !(await new Promise((resolve) => payPrompt.set({ resolve })))) {
    throw new Error(tr('remember.payCancelled'));
  }
  return send();
}

/** Closes the window: true once the password was taken, false when cancelled. */
export function closePayPrompt(ok) {
  payPrompt.update((p) => {
    p?.resolve(!!ok);
    return null;
  });
}
