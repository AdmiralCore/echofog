import { writable } from 'svelte/store';
import { App } from '../api.js';

/** Anonymous mode (GetAnonymity): saved and in effect, and who sees this computer's address now
 *  (seenBy: 'nobody' | 'relay' | 'network'). null until read. */
export const anon = writable(null);

export async function refreshAnon() {
  try {
    anon.set(await App.GetAnonymity());
  } catch (_) {
    /* the node is not started, or an older core */
  }
}
