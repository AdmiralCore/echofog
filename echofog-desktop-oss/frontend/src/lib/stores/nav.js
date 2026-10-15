import { writable } from 'svelte/store';

/** Which main section is open: 'chats' | 'proxy' | 'wallet' | 'settings' */
export const view = writable('chats');

/** A part of a section to bring into view when it opens, e.g. 'bootstrap' in Settings */
export const focus = writable('');
