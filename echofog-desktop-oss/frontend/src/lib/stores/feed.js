import { writable } from 'svelte/store';

/**
 * How a conversation is drawn. "bubbles" (Telegram-like, own messages on the right) suits direct
 * and small chats; "compact" (no bubbles, one dense row per message) keeps big groups readable.
 * The user's choice is 'auto' (default), 'bubbles' or 'compact'; auto picks compact for a channel
 * with more than COMPACT_ABOVE members. Screens call feedMode(pref, memberCount) and put the
 * result on the feed as data-feed="…" (app.css and the gallery show both looks).
 */
const KEY = 'echofog.feed';
export const COMPACT_ABOVE = 50;

function load() {
  try {
    const v = localStorage.getItem(KEY);
    return v === 'bubbles' || v === 'compact' ? v : 'auto';
  } catch (_) {
    return 'auto';
  }
}

export const feedPref = writable(load());
feedPref.subscribe((v) => {
  try { localStorage.setItem(KEY, v); } catch (_) { /* private mode */ }
});

export function feedMode(pref, members = 0) {
  if (pref === 'bubbles' || pref === 'compact') return pref;
  return members > COMPACT_ABOVE ? 'compact' : 'bubbles';
}
