import { writable, derived, get } from 'svelte/store';
import { App, errText } from '../api.js';
import { tr } from '../i18n/index.js';

/** Bookmarked messages, the latest saved first: { id, chatId, chatName, msgId, sender, mine, text, fileName, fileSize, ts, savedAt } */
export const saved = writable([]);
/** IDs ("<chat>/<message>") of the saved messages, for the bookmark marks in chats. */
export const savedIds = derived(saved, (list) => new Set(list.map((m) => m.id)));
export const savedError = writable('');

/** Asks a chat to scroll to a message and flash it: { chatId, msgId }. */
export const jumpTo = writable(null);

export const SAVED_ID = 'saved';

export async function loadSaved() {
  try {
    saved.set((await App.ListSavedMessages()) || []);
  } catch (_) { /* node not started */ }
}

/** Saves or unsaves a message of a chat. */
export async function toggleSaved(conv, m, senderName) {
  savedError.set('');
  const id = conv.id + '/' + m.id;
  try {
    if (get(savedIds).has(id)) {
      await App.UnsaveMessage(id);
      saved.update((list) => list.filter((x) => x.id !== id));
      return false;
    }
    const s = await App.SaveMessage({
      id: '', chatId: conv.id, chatName: conv.name, msgId: m.id, sender: m.mine ? '' : senderName || m.sender || '', mine: !!m.mine,
      text: m.text || '', fileName: m.file?.name || '', fileSize: m.file?.size || 0, ts: m.ts || 0, savedAt: 0,
    });
    saved.update((list) => [s, ...list.filter((x) => x.id !== s.id)]);
    return true;
  } catch (err) {
    savedError.set(errText(err, tr('saved.err')));
    return null;
  }
}

export async function unsave(id) {
  try {
    await App.UnsaveMessage(id);
    saved.update((list) => list.filter((x) => x.id !== id));
  } catch (err) {
    savedError.set(errText(err, tr('saved.err')));
  }
}

/** Writes a message's text to a file the user picks. */
export async function saveAsFile(m, chatName) {
  const stamp = new Date(m.ts || Date.now()).toISOString().slice(0, 16).replace(/[T:]/g, '-');
  const name = `${(chatName || 'message').replace(/[\\/:*?"<>|]+/g, '_').slice(0, 40)}-${stamp}.txt`;
  try {
    return await App.SaveTextFile(name, m.text || '');
  } catch (err) {
    savedError.set(errText(err, tr('saved.err')));
    return '';
  }
}
