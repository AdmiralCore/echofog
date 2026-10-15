import { writable, derived } from 'svelte/store';

/**
 * Colour theme. The choice is 'system' (follow the OS), 'light' or 'dark'; `resolvedTheme` is what
 * is actually shown. It lands on <html data-theme="…">, which app.css keys every colour on.
 * index.html applies the saved choice before the first paint, so there is no flash on start.
 */
const KEY = 'echofog.theme';

function load() {
  try {
    const v = localStorage.getItem(KEY);
    return v === 'light' || v === 'dark' ? v : 'system';
  } catch (_) {
    return 'system';
  }
}

const media = typeof window !== 'undefined' && window.matchMedia ? window.matchMedia('(prefers-color-scheme: light)') : null;
const systemTheme = writable(media?.matches ? 'light' : 'dark');
media?.addEventListener?.('change', (e) => systemTheme.set(e.matches ? 'light' : 'dark'));

export const theme = writable(load());
export const resolvedTheme = derived([theme, systemTheme], ([t, sys]) => (t === 'system' ? sys : t));

let first = true;
let animTimer;
resolvedTheme.subscribe((t) => {
  if (typeof document === 'undefined') return;
  const root = document.documentElement;
  if (!first && root.dataset.theme !== t) {
    root.classList.add('theme-anim');
    clearTimeout(animTimer);
    animTimer = setTimeout(() => root.classList.remove('theme-anim'), 400);
  }
  first = false;
  root.dataset.theme = t;
});

theme.subscribe((t) => {
  try { localStorage.setItem(KEY, t); } catch (_) { /* private mode */ }
});

/** flips between light and dark (leaving 'system' for an explicit choice) */
export function toggleTheme() {
  let current = 'dark';
  resolvedTheme.subscribe((t) => (current = t))();
  theme.set(current === 'dark' ? 'light' : 'dark');
}
