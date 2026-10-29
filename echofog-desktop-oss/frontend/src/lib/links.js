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
// rides after "#", which never reaches the page's server.
export const WEB_LINK_BASE = 'https://admiralcore.github.io/echofog/join/#';

/** The echofog:// link an https page carries after "#" (any page: a fork has its own); other text as it is. */
export function fromWebLink(text) {
  const t = (text || '').trim();
  const m = /^https?:\/\/[^#\s]+#(.+)$/i.exec(t);
  if (!m) return text;
  if (/^echofog:\/\//i.test(m[1])) return m[1];
  if (/^(join\?|relay\/)/i.test(m[1])) return 'echofog://' + m[1];
  return text;
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
export const looksLikeLink = (text) => /^(echofog:\/\/|fogcontact1:|fogroup[12]:)/i.test(fromWebLink((text || '').trim()).trim());

/** The open-link dialog: null when closed, else { text } (the text may be empty). */
export const openLink = writable(null);

export const openLinkDialog = (text = '') => openLink.set({ text });
