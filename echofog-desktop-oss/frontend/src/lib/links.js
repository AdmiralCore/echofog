import { writable } from 'svelte/store';
import { parseContactInput, parseRelayLink } from './contact.js';

// Every kind of EchoFog link the app understands, so that one "open link" dialog takes any of them
// (pasted, scanned, or clicked in a browser, which starts the app with it):
//   echofog://join?…            a node: public (an address) or hidden (it carries an access secret)
//   echofog://relay/host:port/id  a node by its address (a relay, an exit, a node to enter through)
//   host:port/<peer id>          the same without the prefix
//   fogcontact1:… or a 64-hex key  a person to add as a contact
//   fogroup1:… / fogroup2:…      an invitation to a group

// The https form of a link (see site/join): messengers make it tappable, and the EchoFog link
// rides after "#", which never reaches the page's server. Contacts and group invitations have one
// too: echofog://contact/… and echofog://group/… stand for fogcontact1:… and fogroup2:…, so the
// page (and the OS, which hands echofog:// links to the app) can open them.
export const WEB_LINK_BASE = 'https://admiralcore.github.io/echofog/join/#';

/** The longest link the app takes (a v2 card link with its secret fits); the core has the same limit. */
export const MAX_LINK = 16 * 1024;

// an EchoFog link somewhere in a text (a messenger adds words around it, or a dot after it)
const FIND = /(https?:\/\/[^\s#]+#\S+|echofog:\/\/\S+|fogcontact1:\S+|fogroup[12]:\S+)/i;

// A terminal wraps a long line, so a link copied from it carries the breaks inside: the text then
// begins with the whole link, which is joined back before it is looked for.
const JOIN_START = /^(?:echofog:\/\/join\/?\?|https?:\/\/[^\s#]+\/join\/#join\/?\?)/i;
const WHITESPACE = /[ \t\r\n]+/g;

function joinWrapped(text) {
  // a break after every character doubles the text at most: anything longer cannot be one link
  if (text.length > 2 * MAX_LINK || !JOIN_START.test(text)) return text;
  return text.replace(WHITESPACE, '');
}

/**
 * The EchoFog link a text holds, in the app's own form: echofog://join… and echofog://relay/…,
 * fogcontact1:… for a contact, fogroup1:/fogroup2:… for a group. It takes the https form from any
 * page (a fork has its own), a link that was percent-encoded on the way, a link with words
 * around it, and the line breaks a terminal put into a pasted link. Other text comes back as it is.
 */
export function fromWebLink(text) {
  const raw = joinWrapped((text || '').trim());
  const found = FIND.exec(raw);
  if (!found) return text;
  let t = found[1].replace(/[.,;:!?)\]»"'”’>]+$/, '');
  const web = /^https?:\/\/[^#\s]+#(.+)$/i.exec(t);
  if (web) {
    let frag = web[1];
    if (/^(echofog|fogroup[12]|fogcontact1)%3A/i.test(frag)) {
      try { frag = decodeURIComponent(frag); } catch (_) { /* as it is */ }
    }
    if (/^(echofog:\/\/|fogroup[12]:|fogcontact1:)/i.test(frag)) t = frag;
    else if (/^(join\/?\?|relay\/|group\/|contact\/)/i.test(frag)) t = 'echofog://' + frag;
    else return text;
  }
  // a browser or the OS may hand the app echofog://join/?… (a "/" before the "?"): the same link
  t = t.replace(/^(echofog:\/\/join)\/\?/i, '$1?');
  const app = /^echofog:\/\/(group|contact)\/([A-Za-z0-9_-]+)\/?$/i.exec(t);
  if (app) return (app[1].toLowerCase() === 'group' ? 'fogroup2:' : 'fogcontact1:') + app[2];
  return t;
}

/** The https form of a link to hand people (a page that opens the app): '' for other text. */
export function webLinkOf(link) {
  const t = (link || '').trim();
  let m;
  if ((m = /^fogroup2:([A-Za-z0-9_-]+)$/.exec(t))) return WEB_LINK_BASE + 'group/' + m[1];
  if ((m = /^fogcontact1:([A-Za-z0-9_-]+)$/.exec(t))) return WEB_LINK_BASE + 'contact/' + m[1];
  if ((m = /^echofog:\/\/(.+)$/i.exec(t))) return WEB_LINK_BASE + m[1];
  return '';
}

/** What a pasted text is: { kind: 'join' | 'node' | 'contact' | 'group', … } or null. */
export function classifyLink(text) {
  text = fromWebLink((text || '').trim()).trim();
  if (!text || text.length > MAX_LINK) return null;
  if (/^echofog:\/\/join\?/i.test(text)) return { kind: 'join', text };
  if (/^fogroup[12]:/i.test(text)) return { kind: 'group', text };
  const node = parseRelayLink(text);
  if (node) return { kind: 'node', text, peerId: node.peerId, addr: node.addr };
  const c = parseContactInput(text);
  if (c) return { kind: 'contact', text, key: c.key, name: c.name };
  return null;
}

/** Whether the text looks like a link worth offering to open (not a bare key typed in a chat). */
export const looksLikeLink = (text) => {
  const t = (text || '').trim();
  if (t.length >= 64 * 1024) return false;
  const l = fromWebLink(t).trim();
  return l.length <= MAX_LINK && /^(echofog:\/\/|fogcontact1:|fogroup[12]:)/i.test(l);
};

/** The open-link dialog: null when closed, else { text } (the text may be empty). */
export const openLink = writable(null);

// file: a group file (.fogzip) to open, already chosen (one received in a chat)
export const openLinkDialog = (text = '', file = '') => openLink.set({ text, file });

/** The first group invitation in a message's text, in the app's own form (fogroup2:…); '' none. */
export function findGroupLink(text) {
  const s = String(text || '');
  if (!/fogroup|group\/|group%2F/i.test(s)) return '';
  for (const w of s.split(/\s+/)) {
    const m = FIND.exec(w);
    if (!m) continue;
    const t = fromWebLink(m[1].replace(/[.,;:!?)\]»"']+$/, ''));
    if (/^fogroup[12]:/i.test(t)) return t;
  }
  return '';
}

/**
 * Which version an echofog://join link is and whether it carries a secret (an invitation):
 * { version, hasSecret }, or null when the text is not a join link. Version 1 has no "v"; "v=2" is
 * the card link of protocol 2; a "v" that is not a number gives 0. The secret itself is never
 * returned, so the result is safe to show or log.
 */
export function joinInfo(text) {
  if (typeof text !== 'string') return null;
  const t = fromWebLink(text.trim()).trim();
  if (!t || t.length > MAX_LINK) return null;
  const m = /^echofog:\/\/join\?(.*)$/is.exec(t);
  if (!m) return null;
  const q = new URLSearchParams(m[1]);
  const v = q.get('v');
  const version = v === null ? 1 : /^\d{1,3}$/.test(v) ? Number(v) : 0;
  return { version, hasSecret: q.has('s') || q.has('token') };
}
