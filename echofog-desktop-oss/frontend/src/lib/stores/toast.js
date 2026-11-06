import { writable } from 'svelte/store';

/**
 * Short messages that appear in a corner and go away by themselves (Toast.svelte shows them).
 *
 *   toast({ kind: 'success', message: tr('common.saved') });
 *   toast({ kind: 'error', message, action: { label: tr('common.refresh'), run: () => … }, timeout: 0 });
 *
 * kind: info | success | warning | error. timeout in ms; 0 keeps it until dismissed.
 * Errors and warnings stay longer than the rest.
 */
export const toasts = writable([]); // [{ id, kind, message, action, timeout }]

let seq = 0;
const timers = new Map();

export function toast({ kind = 'info', message, action = null, timeout } = {}) {
  const id = ++seq;
  const ms = timeout ?? (kind === 'error' ? 8000 : kind === 'warning' ? 6000 : 4000);
  toasts.update((l) => [...l.slice(-3), { id, kind, message, action, timeout: ms }]);
  if (ms > 0) timers.set(id, setTimeout(() => dismissToast(id), ms));
  return id;
}

export function dismissToast(id) {
  clearTimeout(timers.get(id));
  timers.delete(id);
  toasts.update((l) => l.filter((x) => x.id !== id));
}
