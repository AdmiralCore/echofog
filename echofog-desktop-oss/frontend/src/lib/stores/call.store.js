import { writable, derived, get } from 'svelte/store';
import { callService, callState } from '../services/call.service.js';
import { InviteToCall, GetContacts, EndGroupCall } from '@wailsjs/go/main/App';
import { EventsOn } from '@wailsjs/runtime/runtime';
import { loadPeerDirectory } from './peers.store.js';

/** What the call is about, for headers and the mini player: { alias, avatar, peerId, chatId } */
export const peerInfo = writable({ alias: '', avatar: '', peerId: '', chatId: '' });
export const incomingCall = writable(null);
export const callDuration = writable(0);
/** Whether the full call stage is shown in the chat (vs. only the mini player). */
export const stageExpanded = writable(true);

export const isCallActive = derived(callState, ($s) => $s.inCall || $s.connecting);

export const callStatus = derived(callState, ($s) => {
    if ($s.inCall) return 'active';
    if ($s.connecting) return 'connecting';
    if ($s.error) return 'failed';
    return 'idle';
});

export const callError = derived(callState, ($s) => $s.error);

export const callControls = derived(callState, ($s) => ({
    isMuted: $s.isMicMuted,
    isDeafened: $s.isDeafened,
    isVideoOn: $s.isCameraOn,
    isScreenSharing: $s.isScreenSharing,
}));

let timerInterval = null;
let eventsReady = false;

callState.subscribe(($s) => {
    if ($s.inCall && !timerInterval) {
        callDuration.set(0);
        timerInterval = setInterval(() => callDuration.update((n) => n + 1), 1000);
    } else if (!$s.inCall && timerInterval) {
        clearInterval(timerInterval);
        timerInterval = null;
    }
});

export function formatDuration(seconds) {
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60).toString().padStart(2, '0');
    const s = (seconds % 60).toString().padStart(2, '0');
    return h ? `${h}:${m}:${s}` : `${m}:${s}`;
}

function errMessage(err) {
    return (err && (err.message || String(err))) || 'Call failed';
}

// --- ringtone: two soft tones, generated so no audio asset is needed -------------------------
let ring = null;
function startRingtone() {
    stopRingtone();
    try {
        const ctx = callService.ensureAudioContext();
        const gain = ctx.createGain();
        gain.gain.value = 0;
        gain.connect(ctx.destination);
        const osc = [ctx.createOscillator(), ctx.createOscillator()];
        osc[0].frequency.value = 523.25;
        osc[1].frequency.value = 659.25;
        osc.forEach((o) => { o.type = 'sine'; o.connect(gain); o.start(); });
        let on = false;
        const timer = setInterval(() => {
            on = !on;
            gain.gain.setTargetAtTime(on ? 0.06 : 0, ctx.currentTime, 0.05);
        }, 700);
        ring = { osc, gain, timer };
    } catch (_) { /* audio unavailable */ }
}

function stopRingtone() {
    if (!ring) return;
    clearInterval(ring.timer);
    ring.osc.forEach((o) => { try { o.stop(); } catch (_) { /* stopped */ } });
    ring.gain.disconnect();
    ring = null;
}

export function initCallEvents() {
    if (eventsReady) return;
    eventsReady = true;
    callService.ensureEvents();
    try {
        EventsOn('call:incoming', async (ev) => {
            if (!ev || get(callState).inCall || get(callState).connecting) return;
            let alias = '';
            try {
                const contacts = (await GetContacts()) || [];
                alias = contacts.find((c) => c.peer_id === ev.from)?.alias || '';
            } catch (_) { /* contacts unavailable */ }
            incomingCall.set({
                callId: ev.callId,
                coordinator: ev.coordinator,
                from: ev.from,
                alias: alias || String(ev.from || '').slice(0, 12),
                timestamp: ev.timestamp,
            });
            startRingtone();
        });
    } catch (err) {
        console.warn('Call events unavailable:', err);
    }
}

/** A call somebody else started (a group call): rings like a direct one. */
export function ringIncoming({ callId, coordinator, from, alias, chatId }) {
    const s = get(callState);
    if (s.inCall || s.connecting || get(incomingCall)) return;
    incomingCall.set({ callId, coordinator, from, alias, chatId, timestamp: Date.now() });
    startRingtone();
}

export async function startCall(peer, withVideo = false) {
    if (get(callState).inCall || get(callState).connecting) return false;
    peerInfo.set({ alias: peer.alias || '', avatar: peer.avatar || '', peerId: peer.peerId || '', chatId: peer.chatId || '' });
    stageExpanded.set(true);
    resumeAudioContext();
    loadPeerDirectory();
    try {
        let callId = peer.callId || peer.groupId || '';
        let coordinator = peer.coordinator || '';
        if (peer.pubKey) {
            const invite = await InviteToCall(peer.pubKey);
            callId = invite.callId;
            coordinator = invite.coordinator;
        }
        if (!callId) throw new Error('Nothing to call');
        await callService.joinCall(callId, coordinator);
        if (withVideo) await callService.startCamera();
        return true;
    } catch (err) {
        callState.update((s) => ({ ...s, error: errMessage(err) }));
        return false;
    }
}

export async function acceptIncomingCall(withVideo = false) {
    const inv = get(incomingCall);
    if (!inv) return false;
    stopRingtone();
    incomingCall.set(null);
    peerInfo.set({ alias: inv.alias, avatar: '', peerId: inv.from, chatId: inv.chatId || 'd:' + inv.from });
    stageExpanded.set(true);
    resumeAudioContext();
    loadPeerDirectory();
    try {
        await callService.joinCall(inv.callId, inv.coordinator);
        if (withVideo) await callService.startCamera();
        return true;
    } catch (err) {
        callState.update((s) => ({ ...s, error: errMessage(err) }));
        return false;
    }
}

export function declineIncomingCall() {
    stopRingtone();
    incomingCall.set(null);
}

export async function endCall() {
    // the last person to leave a group call ends it for the group (stops it ringing, removes "join")
    const st = get(callState);
    const chat = get(peerInfo).chatId || '';
    if (chat.startsWith('g:') && st.callId && (st.inCall || st.connecting) && st.members.length <= 1) {
        try { await EndGroupCall(chat.slice(2), st.callId); } catch (_) { /* the group may be gone */ }
    }
    await callService.leaveCall();
    callState.update((s) => ({ ...s, error: null }));
}

export function toggleMute() {
    resumeAudioContext();
    callService.toggleMic();
}

export function toggleDeafen() {
    resumeAudioContext();
    callService.toggleDeafen();
}

export async function toggleVideo() {
    resumeAudioContext();
    await callService.toggleCamera();
}

export async function toggleScreenShare() {
    resumeAudioContext();
    await callService.toggleScreenShare(get(callState).selectedQualityId);
}

// Browsers only start audio from a user gesture; every call control routes through here.
export function resumeAudioContext() {
    return callService.ensureAudioContext();
}

export function getAudioContext() {
    return callService.audioCtx;
}

export async function panicClear() {
    declineIncomingCall();
    callDuration.set(0);
    await callService.leaveCall();
    callState.update((s) => ({ ...s, error: null }));
}
