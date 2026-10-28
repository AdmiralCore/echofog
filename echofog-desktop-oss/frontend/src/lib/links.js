import { writable } from 'svelte/store';
import { parseContactInput, parseRelayLink } from './contact.js';

// Every kind of EchoFog link the app understands, so that one "open link" dialog takes any of them
// (pasted, scanned, or clicked in a browser, which starts the app with it):
//   echofog://join?…            a node: public (an address) or hidden (it carries an access secret)
//   echofog://relay/host:port/id  a node by its address (a relay, an exit, a node to enter through)
//   host:port/<peer id>          the same without the prefix
//   fogcontact1:… or a 64-hex key  a person to add as a contact
//   fogroup1:… / fogroup2:…      an invitation to a group

/** What a pasted text is: { kind: 'join' | 'node' | 'contact' | 'group', … } or null. */
export function classifyLink(text) {
  text = (text || '').trim();
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
export const looksLikeLink = (text) => /^(echofog:\/\/|fogcontact1:|fogroup[12]:)/i.test((text || '').trim());

/** The open-link dialog: null when closed, else { text } (the text may be empty). */
export const openLink = writable(null);

export const openLinkDialog = (text = '') => openLink.set({ text });
