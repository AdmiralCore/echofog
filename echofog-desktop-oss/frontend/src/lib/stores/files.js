import { writable } from 'svelte/store';
import { App, on, errText } from '../api.js';
import { tr } from '../i18n/index.js';

/** File transfers by ID: { id, peer, dir: 'in'|'out', name, size, state, error, path, done, rate, stalled, created } */
export const transfers = writable({});
/** The last thing that went wrong with a file, shown under the chat. */
export const fileError = writable('');

let listening = false;

function put(t) {
  if (!t || !t.id) return;
  transfers.update((m) => ({ ...m, [t.id]: t }));
}

function listen() {
  if (listening) return;
  listening = true;
  on('file:transfer', put);
}

/** Loads the transfers with a contact (all of them for ''). */
export async function loadTransfers(peerId = '') {
  listen();
  try {
    const list = (await App.ListFileTransfers(peerId)) || [];
    transfers.update((m) => {
      const next = { ...m };
      for (const t of list) next[t.id] = t;
      return next;
    });
  } catch (_) { /* node not started */ }
}

async function act(fn) {
  fileError.set('');
  try {
    const r = await fn();
    if (r && r.id) put(r);
    return r;
  } catch (err) {
    fileError.set(errText(err, tr('files.err.generic')));
    return null;
  }
}

/** Lets the user pick a file and offers it to the contact. */
export const pickAndSend = (conv) => act(() => App.PickAndSendFile(conv.pubKey));
/** Offers files dropped on the chat. */
export async function sendPaths(conv, paths) {
  for (const p of paths || []) await act(() => App.SendFile(conv.pubKey, p));
}
export const accept = (id) => act(() => App.AcceptFile(id));
export const decline = (id) => act(async () => { await App.DeclineFile(id); return refresh(id); });
export const cancel = (id) => act(async () => { await App.CancelFile(id); return refresh(id); });
export const showInFolder = (id) => act(() => App.ShowTransferFile(id));

/** Opens a received file; a file that runs a program opens only after the user confirms. */
export async function openFile(t) {
  let risky = false;
  try { risky = await App.IsRiskyFile(t.name); } catch (_) { /* old core: ask anyway below */ }
  if (risky && !window.confirm(tr('files.confirmRisky', { name: t.name }))) return;
  await act(() => App.OpenTransferFile(t.id, risky));
}

async function refresh(id) {
  const list = (await App.ListFileTransfers('')) || [];
  return list.find((t) => t.id === id) || null;
}

export function clearTransfers() {
  transfers.set({});
}

// share of a transfer done, 0..1
export const progress = (t) => (t && t.size > 0 ? Math.min(1, t.done / t.size) : t && t.state === 'done' ? 1 : 0);

/** Seconds left at the current rate, or 0 when unknown. */
export function eta(t) {
  if (!t || !t.rate || t.state !== 'active') return 0;
  return Math.max(0, Math.round((t.size - t.done) / t.rate));
}
