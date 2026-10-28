import { derived } from 'svelte/store';
import { chats } from './chats.js';
import { short } from '../format.js';

/** Names people: a contact's alias, else the start of the peer id. The one place that does it. */
export const nameOf = derived(chats, ($chats) => {
  const names = new Map($chats.filter((c) => c.kind === 'direct').map((c) => [c.peerId, c.name]));
  return (id) => names.get(id) || (id ? short(id, 4) : '');
});
