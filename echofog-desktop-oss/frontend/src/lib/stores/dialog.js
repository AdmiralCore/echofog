import { writable } from 'svelte/store';
import { tr } from '../i18n/index.js';

/**
 * The app's own confirmation dialog, in place of window.confirm: the system one looks foreign in
 * the window and names "wails.localhost" as the asker.
 *
 *   if (!(await ask({ message: '…', confirm: tr('common.delete'), danger: true }))) return;
 */
export const dialog = writable(null); // { title, message, confirm, cancel, danger, resolve }

export function ask({ title = '', message, confirm = '', cancel = '', danger = false }) {
  return new Promise((resolve) => {
    dialog.update((prev) => {
      prev?.resolve(false); // a dialog already open is answered "no"
      return { title, message, confirm: confirm || tr('dialog.ok'), cancel: cancel || tr('common.cancel'), danger, resolve };
    });
  });
}

/** Closes the open dialog with the answer. */
export function answer(yes) {
  dialog.update((d) => {
    d?.resolve(!!yes);
    return null;
  });
}
