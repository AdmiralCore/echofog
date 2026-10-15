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

// A relay link names a node by the address others reach it at: "echofog://relay/host:port/<peer id>".
// It lets someone use a private relay (which does not advertise itself) or a friend's exit.
const RELAY = 'echofog://relay/';
const PEER_ADDR = /^(.+)\/([0-9a-fA-F]{64})\/?$/;

/** Reads a relay link or "host:port/<peer id>" (or a ws:// URL ending in the id): { addr, peerId } or null. */
export function parseRelayLink(text) {
  text = (text || '').trim();
  if (text.toLowerCase().startsWith(RELAY)) text = text.slice(RELAY.length);
  const m = PEER_ADDR.exec(text);
  if (!m) return null;
  const addr = m[1];
  if (!/^wss?:\/\/./.test(addr) && !/^(\[[0-9a-fA-F:.]+\]|[^\s/:]+):\d{1,5}$/.test(addr)) return null;
  return { addr, peerId: m[2].toLowerCase() };
}

export const makeRelayLink = (addr) => RELAY + addr;

async function sha256Hex(hexKey) {
  const bytes = new Uint8Array(hexKey.match(/../g).map((h) => parseInt(h, 16)));
  const d = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
  return [...d].map((b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Turns what a person pasted into a peer ID: a peer ID, a contact's key, or a contact link (the
 * peer ID is the SHA-256 of the key). Returns '' when it is none of them.
 */
export async function peerIdFrom(text, contacts = []) {
  text = (text || '').trim();
  const relay = parseRelayLink(text);
  if (relay) return relay.peerId;
  const c = parseContactInput(text);
  if (!c) return '';
  const known = contacts.find((x) => (x.pub_key || '').toLowerCase() === c.key || (x.peer_id || '').toLowerCase() === c.key);
  if (known) return known.peer_id.toLowerCase();
  if (text.startsWith(SCHEME)) return sha256Hex(c.key); // a link always carries the key
  return c.key; // a bare 64-hex value: taken as the peer ID
}
