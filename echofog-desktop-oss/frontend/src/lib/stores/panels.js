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
 * The "you" card stands at the foot of a side column (the personal list, or a space's channels) and
 * the rail holds one too: two avatars with the same name would say it twice. Each column reports its
 * own card here, and the rail keeps its card only while none of them reports one.
 */
export const selfCards = writable({ personal: false, space: false });

/** Whether the personal list on screen carries the "you" card. `off` is a window with no room for a
 *  strip: the column is forced in place there, whatever the remembered fold says. */
export const personalCardOnScreen = (view, showAside, hidden, off) => view === 'chats' && showAside && (off || !hidden);

/** Whether a space's column of channels on screen carries one. */
export const spaceCardOnScreen = (view, kind, columnShown) => view === 'chats' && kind === 'group' && columnShown;

/** Whether the rail shows its own card: only while no column reports one. */
export const railCardShown = (cards) => !cards.personal && !cards.space;

/** A column's report; the same value again keeps the state, so the rail is not woken in vain. */
export function setSelfCard(kind, onScreen) {
  const on = !!onScreen;
  selfCards.update((cards) => (cards[kind] === on ? cards : { ...cards, [kind]: on }));
}
