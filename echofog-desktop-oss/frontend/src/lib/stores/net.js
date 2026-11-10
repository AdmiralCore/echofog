import { writable } from 'svelte/store';

/** The last network status the shell read from the core: { status, peerCount }. The shell polls it;
 *  other screens (the "you" card of the channels column) only read it. */
export const netStatus = writable({ status: 'OFFLINE', peerCount: 0 });
