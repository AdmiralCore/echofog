import { writable, derived, get } from 'svelte/store';
import { GetContacts, GetMyID } from '@wailsjs/go/main/App';
import { lang, tr } from '../i18n/index.js';

// Who is who: our own peer ID and the contact book, used to label call members and messages.
export const selfId = writable('');
export const contactsById = writable({});

export async function loadPeerDirectory() {
    try {
        selfId.set((await GetMyID()) || '');
    } catch (_) { /* node not started yet */ }
    try {
        const map = {};
        for (const c of (await GetContacts()) || []) map[c.peer_id] = c;
        contactsById.set(map);
    } catch (_) { /* contacts unavailable */ }
}

export function shortId(id) {
    return id ? `${id.slice(0, 4)}…${id.slice(-4)}` : '';
}

/** peerName returns a human label for a peer ID. */
export const peerName = derived([selfId, contactsById, lang], ([$self, $contacts]) => (id) => {
    if (!id) return tr('chats.unknown');
    if (id === $self) return tr('chats.You');
    return $contacts[id]?.alias || shortId(id);
});

export const peerAvatar = derived(contactsById, ($contacts) => (id) => $contacts[id]?.avatar_base64 || '');

// Stable per-person colour, like Telegram's avatar palette.
const PALETTE = ['#e17076', '#7bc862', '#65aadd', '#a695e7', '#ee7aae', '#6ec9cb', '#faa774', '#5ea1e0'];
export function colorFor(id = '') {
    // FNV-1a: similar IDs (hex keys sharing a prefix) still spread across the palette
    let h = 0x811c9dc5;
    for (let i = 0; i < id.length; i++) {
        h ^= id.charCodeAt(i);
        h = Math.imul(h, 0x01000193) >>> 0;
    }
    return PALETTE[h % PALETTE.length];
}

export function initials(name = '') {
    const parts = name.replace(/[^\p{L}\p{N} ]/gu, ' ').trim().split(/\s+/).filter(Boolean);
    if (!parts.length) return '?';
    return (parts[0][0] + (parts[1]?.[0] || '')).toUpperCase();
}

export function nameOf(id) {
    return get(peerName)(id);
}
