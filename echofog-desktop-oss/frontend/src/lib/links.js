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

// an EchoFog link somewhere in a text (a messenger adds words around it, or a dot after it)
const FIND = /(https?:\/\/[^\s#]+#\S+|echofog:\/\/\S+|fogcontact1:\S+|fogroup[12]:\S+)/i;

/**
 * The EchoFog link a text holds, in the app's own form: echofog://join… and echofog://relay/…,
 * fogcontact1:… for a contact, fogroup1:/fogroup2:… for a group. It takes the https form from any
 * page (a fork has its own), a link that was percent-encoded on the way, and a link with words
 * around it. Other text comes back as it is.
 */
export function fromWebLink(text) {
  const raw = (text || '').trim();
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
    else if (/^(join\?|relay\/|group\/|contact\/)/i.test(frag)) t = 'echofog://' + frag;
    else return text;
  }
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
  if (!text) return null;
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
  return t.length < 64 * 1024 && /^(echofog:\/\/|fogcontact1:|fogroup[12]:)/i.test(fromWebLink(t).trim());
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
