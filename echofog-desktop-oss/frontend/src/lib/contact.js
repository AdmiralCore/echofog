import { writable, get } from 'svelte/store';

// A contact link carries the name and the key, so that the other person does not have to type
// either: "fogcontact1:" + base64url of {"n": name, "k": key}. A bare 64-character key works too.
const SCHEME = 'fogcontact1:';
const KEY = /^[0-9a-fA-F]{64}$/;

function b64(s) {
  return btoa(unescape(encodeURIComponent(s))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function unb64(s) {
  const pad = s.length % 4 ? '='.repeat(4 - (s.length % 4)) : '';
  return decodeURIComponent(escape(atob(s.replace(/-/g, '+').replace(/_/g, '/') + pad)));
}

export function makeContactLink(name, key) {
  return SCHEME + b64(JSON.stringify({ n: (name || '').trim().slice(0, 64), k: key }));
}

/** Reads what a person pasted or scanned: { key, name } or null when it is neither link nor key. */
export function parseContactInput(text) {
  text = (text || '').trim();
  if (KEY.test(text)) return { key: text.toLowerCase(), name: '' };
  if (text.startsWith(SCHEME)) {
    try {
      const v = JSON.parse(unb64(text.slice(SCHEME.length)));
      if (v && KEY.test(v.k || '')) return { key: v.k.toLowerCase(), name: String(v.n || '').trim() };
    } catch (_) { /* not a link */ }
  }
  return null;
}

const NAME_KEY = 'echofog.displayName';

function loadName() {
  try { return localStorage.getItem(NAME_KEY) || ''; } catch (_) { return ''; }
}

/** The name shown to contacts who add this person; empty means the profile name is used. */
export const displayName = writable(loadName());
displayName.subscribe((v) => {
  try { localStorage.setItem(NAME_KEY, v || ''); } catch (_) { /* private mode */ }
});

export const currentDisplayName = (profileName) => (get(displayName) || '').trim() || profileName || 'me';
