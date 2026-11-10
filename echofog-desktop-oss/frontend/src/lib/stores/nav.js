import { writable } from 'svelte/store';

/** Which main section is open: 'chats' | 'proxy' | 'wallet' | 'settings' */
export const view = writable('chats');

/** A part of a section to bring into view when it opens, e.g. 'bootstrap' in Settings */
export const focus = writable('');

/** A thing for the chat list to start: "group" (new space) or "join" (join by invite); the list clears it */
export const chatsAction = writable("");
