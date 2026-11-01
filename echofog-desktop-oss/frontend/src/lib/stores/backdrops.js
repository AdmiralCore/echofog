import { writable, derived } from 'svelte/store';
import { App, on } from '../api.js';
import { callState } from '../services/call.service.js';
import { selfId } from './peers.store.js';
import { log, errAttrs } from '../uilog.js';

// Animated backgrounds of call tiles without a camera. Each person picks one for their own tile
// and everybody in the call sees it: the name rides in the person's call profile (core:
// tree.PeerNodeProfile.Backdrop). A name this build does not know draws the plain look.

/** in picker order; '' is the plain look */
export const BACKDROPS = ['', 'gradient', 'waves', 'particles', 'stars', 'cubes', 'matrix'];

const KEY = 'echofog.backdrop';

function load() {
    try {
        const v = JSON.parse(localStorage.getItem(KEY) || '{}');
        return { mine: BACKDROPS.includes(v.mine) ? v.mine : '', others: v.others !== false };
    } catch (_) {
        return { mine: '', others: true };
    }
}

/** { mine: the backdrop of my tile, others: show the ones other people chose }, remembered between runs */
export const backdropSettings = writable(load());
backdropSettings.subscribe((v) => {
    try { localStorage.setItem(KEY, JSON.stringify(v)); } catch (_) { /* private mode */ }
});

/** peer -> backdrop, from the call roster; listened to while some tile shows */
const members = writable({}, (set) => {
    const apply = (r) => set((r && r.callId && r.backdrops) || {});
    App.GetCallRoster().then(apply).catch(() => {});
    return on('call:roster', apply);
});

const self = derived(callState, (s) => s.self);

/** backdropOf(peer): what to draw on that person's tile */
export const backdropOf = derived([backdropSettings, members, self], ([$s, $members, $self]) => (peer) => {
    if (peer === $self) return $s.mine;
    const b = $members[peer] || '';
    return $s.others && BACKDROPS.includes(b) ? b : '';
});

// The core puts our backdrop in the profile of every call we join. It hears it when the node is
// up, on every change, and again as a call starts (a node started since has not heard it).
const joining = derived(callState, (s) => !!(s.inCall || s.connecting));
const mine = derived(backdropSettings, (s) => s.mine);
derived([mine, selfId, joining], (v) => v).subscribe(([name, id]) => {
    if (!id) return;
    App.SetCallBackdrop(name).catch((e) => log.debug('call', 'the core did not take the tile backdrop', errAttrs(e, { name })));
});
