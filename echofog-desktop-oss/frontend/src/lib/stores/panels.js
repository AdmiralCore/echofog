import { writable, get } from 'svelte/store';

/**
 * The side column (a space's channels, or the list of personal chats) can be hidden; the choice is
 * remembered apart for the two. Hidden, it is a thin strip by the rail that slides the column over
 * the content on hover (PanelHost); a click on the strip pins it back.
 */
const KEYS = { personal: 'echofog.panel.personal', space: 'echofog.panel.space' };

function load(kind) {
  try {
    const v = localStorage.getItem(KEYS[kind]);
    if (v === '1' || v === '0') return v === '1';
    // the old single setting of the chat list
    if (kind === 'personal') return localStorage.getItem('echofog.chatsCollapsed') === '1';
  } catch (_) { /* private mode */ }
  return false;
}

function make(kind) {
  const s = writable(load(kind));
  s.subscribe((v) => {
    try { localStorage.setItem(KEYS[kind], v ? '1' : '0'); } catch (_) { /* not kept */ }
  });
  return s;
}

export const panelHidden = { personal: make('personal'), space: make('space') };

export function togglePanel(kind) {
  panelHidden[kind].update((v) => !v);
}

export const isPanelHidden = (kind) => get(panelHidden[kind]);

/**
 * The "you" card stands at the foot of the personal list and the rail holds one too: two avatars with
 * the same name would say it twice. The list reports its own card here, and the rail keeps its card
 * only while that report is false.
 */
export const selfCardShown = writable(false);

/** Whether a column on screen carries the "you" card: only the personal list does. */
export const selfCardOnScreen = (view, showAside, hidden) => view === 'chats' && showAside && !hidden;
