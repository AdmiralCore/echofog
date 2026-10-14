import { writable } from 'svelte/store';

/** Which main section is open: 'chats' | 'proxy' | 'wallet' | 'settings' */
export const view = writable('chats');
