import { get } from 'svelte/store';
import { callState } from './services/call.service.js';
import { chatError } from './stores/chats.js';
import { incomingCall } from './stores/call.store.js';
import { play, installClickSounds, preload, startRinging } from './sounds.js';

/** Connects sounds to what happens in calls and to errors. Returns a function that undoes it. */
export function bindSounds() {
  preload();
  const offClicks = installClickSounds();

  let prev = get(callState);
  const offCall = callState.subscribe((s) => {
    if (s.inCall && !prev.inCall) play('join');
    else if (!s.inCall && prev.inCall) play('leave');
    else if (s.inCall && prev.inCall && s.isMicMuted !== prev.isMicMuted && !s.isDeafened && !prev.isDeafened) {
      play(s.isMicMuted ? 'muteOn' : 'muteOff');
    } else if (s.inCall && prev.inCall && s.isDeafened !== prev.isDeafened) {
      play(s.isDeafened ? 'muteOn' : 'muteOff');
    }
    if (s.error && s.error !== prev.error) play('error');
    prev = s;
  });

  let lastError = '';
  const offErr = chatError.subscribe((e) => {
    if (e && e !== lastError) play('error');
    lastError = e;
  });

  // a soft tune while a call waits for an answer
  let stopRing = null;
  const offRing = incomingCall.subscribe((inc) => {
    if (inc && !stopRing) stopRing = startRinging();
    else if (!inc && stopRing) { stopRing(); stopRing = null; }
  });

  return () => { offClicks(); offCall(); offErr(); offRing(); if (stopRing) stopRing(); };
}
