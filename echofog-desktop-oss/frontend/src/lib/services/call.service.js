import { writable, get } from 'svelte/store';
import { on as EventsOn } from '../api.js';
import { setSink } from '../sounds.js';
import { tr } from '../i18n/index.js';
import { log, errAttrs } from '../uilog.js';

const PREF = 'echofog.devices';
function loadPrefs() {
  try { return JSON.parse(localStorage.getItem(PREF) || '{}'); } catch (_) { return {}; }
}
function savePrefs(p) {
  try { localStorage.setItem(PREF, JSON.stringify({ ...loadPrefs(), ...p })); } catch (_) { /* private mode */ }
}

// The Go bridge offer has a fixed layout, so tracks are attached with replaceTrack and the
// loopback never renegotiates:
//   m-lines 0..3   UI capture: mic, camera, screen, screen sound (Go receives)
//   then 16 audio  one playback track per remote speaker        (slots a0..a15)
//   then 4 video   one playback track per watched stream        (slots v0..v3)
//   then 4 audio   the sound of each watched stream             (slots s0..s3)
const LOCAL = { mic: 0, camera: 1, screen: 2, screenAudio: 3 };
const LOCAL_LINES = 4;
const AUDIO_SLOTS = 16;
const VIDEO_SLOTS = 4;
const STREAM_AUDIO_SLOTS = 4;
// Screen sound is music and films, not speech: stereo and a bitrate Opus sounds clean at.
const SCREEN_AUDIO_KBPS = 160;
const SPEAKING_LEVEL = 0.035;
const AUTO_WATCH_CAMERAS = 3;
// how often the WebRTC numbers go into the log while in a call
const STATS_EVERY_MS = 60_000;

const app = () => window.go?.main?.App || {};

function freshState(prev = {}) {
  return {
    inCall: false,
    connecting: false,
    error: null,
    callId: null,
    self: prev.self || '',
    coordinator: '',
    members: [],
    isMicMuted: false,
    isDeafened: false,
    isScreenSharing: false,
    isCameraOn: false,
    hasMic: true,
    speaking: {},          // peer -> true while their level is above the threshold
    activeStreams: [],     // [{ streamId, broadcaster, mediaType, bitrate, width, height, fps, qualityId }]
    watching: [],          // stream IDs we receive
    focusedStreamId: null, // stream shown large on the stage
    qualities: prev.qualities || [],
    selectedQualityId: prev.selectedQualityId || '1080p60',
    streamQualityId: null,
    streamError: null,
    slots: {},             // slot -> { kind, peer, streamId }
    peerVolume: prev.peerVolume || {}, // peer -> 0..2 (1 = 100%)
    streamVolume: prev.streamVolume || {}, // stream -> 0..2: the sound of a watched stream
    screenAudio: false,    // our shared screen carries its sound
    localMuted: prev.localMuted || {}, // peer -> true: muted for us only
    reported: {},
    screenStreamId: null,
    cameraStreamId: null,
    devices: prev.devices || { mics: [], cams: [], outs: [] },
    micDeviceId: prev.micDeviceId ?? loadPrefs().mic ?? '',
    camDeviceId: prev.camDeviceId ?? loadPrefs().cam ?? '',
    outDeviceId: prev.outDeviceId ?? loadPrefs().out ?? '',
    // what a shared screen keeps when the encoder is short of time or bitrate: 'motion' its frame
    // rate (games, films), 'detail' its sharpness (text, slides)
    screenMode: prev.screenMode || loadPrefs().screenMode || 'motion',
    notice: null,
  };
}

export const callState = writable(freshState());
/** slot -> MediaStream for remote video, plus 'local:screen' / 'local:camera' previews. */
export const videoStreams = writable({});

class CallService {
  constructor() {
    this.pc = null;
    this.tx = null;
    this.mic = null;
    this.screen = null;
    this.camera = null;
    this.audioCtx = null;
    this.audioSlots = new Map(); // slot -> { gain, analyser, el, data }
    this.selfAnalyser = null;
    this.levelTimer = null;
    this.eventsReady = false;
    this.lastSelfSpeaking = false;
    this.noticeTimer = null;
    this.muteOutput = false; // analyse but do not play remote audio (silent previews)
    // a headset plugged in or removed: the lists follow
    try { navigator.mediaDevices?.addEventListener('devicechange', () => this.refreshDevices()); } catch (_) { /* no media devices */ }
  }

  // --- lifecycle -------------------------------------------------------------------------

  async joinCall(callId, coordPeerId = '') {
    if (this.pc) return true;
    callState.update((s) => ({ ...freshState(s), connecting: true, callId }));
    this.ensureEvents();
    log.info('call', 'joining the call', { callId, coordinator: short(coordPeerId) });
    try {
      this.ensureAudioContext();
      this.mic = await this.openMic().catch((e) => {
        log.warn('call', 'microphone did not open, joining without it', errAttrs(e, { device: get(callState).micDeviceId ? 'chosen' : 'default' }));
        return null;
      });
      callState.update((s) => ({ ...s, hasMic: !!this.mic, isMicMuted: !this.mic }));

      const pc = new RTCPeerConnection({ iceServers: [] });
      this.pc = pc;
      pc.ontrack = (ev) => this.onTrack(ev);
      this.watchConnection(pc);

      if (!app().JoinRoom) throw new Error(tr('call.err.unavailable'));
      const offer = await app().JoinRoom(callId, coordPeerId);
      // ask the browser to send the screen's sound in stereo (the bridge forwards it untouched)
      await pc.setRemoteDescription({ type: 'offer', sdp: opusStereo(offer, [LOCAL.screenAudio]) });

      const t = pc.getTransceivers();
      if (t.length < LOCAL_LINES + AUDIO_SLOTS + VIDEO_SLOTS) throw new Error(tr('call.err.bridge'));
      this.tx = { mic: t[LOCAL.mic], camera: t[LOCAL.camera], screen: t[LOCAL.screen], screenAudio: t[LOCAL.screenAudio] };
      for (const role of ['mic', 'camera', 'screen', 'screenAudio']) this.tx[role].direction = 'sendonly';
      this.preferH264(this.tx.camera);
      this.preferH264(this.tx.screen);
      if (this.mic) await this.tx.mic.sender.replaceTrack(this.mic.getAudioTracks()[0]);

      // and to decode the sound of watched streams in stereo
      const answer = await pc.createAnswer();
      const first = LOCAL_LINES + AUDIO_SLOTS + VIDEO_SLOTS;
      const streamLines = Array.from({ length: STREAM_AUDIO_SLOTS }, (_, i) => first + i);
      await pc.setLocalDescription({ type: 'answer', sdp: opusStereo(answer.sdp, streamLines) });
      await waitIceGathering(pc, 1500);
      await app().SubmitCallAnswer(pc.localDescription.sdp);

      this.startLevelMonitor();
      this.startStatsLog();
      log.info('call', 'joined the call', { callId, mic: !!this.mic, transceivers: t.length, ice: pc.iceGatheringState });
      callState.update((s) => ({ ...s, inCall: true, connecting: false }));
      this.refreshDevices();
      this.loadQualities();
      this.syncFromCore();
      return true;
    } catch (err) {
      log.error('call', 'joining the call failed', errAttrs(err, { callId }));
      const message = String(err?.message || err);
      await this.leaveCall();
      callState.update((s) => ({ ...s, error: message }));
      throw err;
    }
  }

  async leaveCall() {
    clearInterval(this.levelTimer);
    this.levelTimer = null;
    clearInterval(this.statsTimer);
    this.statsTimer = null;
    if (this.pc) {
      await this.logStats('at leaving').catch(() => {});
      log.info('call', 'leaving the call', { callId: get(callState).callId });
    }
    for (const s of [this.mic, this.screen, this.camera]) s?.getTracks().forEach((t) => { t.onended = null; t.stop(); });
    this.mic = this.screen = this.camera = null;
    for (const a of this.audioSlots.values()) {
      try { a.el.srcObject = null; a.source.disconnect(); } catch (_) { /* already gone */ }
    }
    this.audioSlots.clear();
    this.selfAnalyser = null;
    if (this.pc) {
      try { this.pc.close(); } catch (_) { /* closed */ }
      this.pc = null;
    }
    this.tx = null;
    const wasIn = get(callState).inCall || get(callState).connecting;
    if (wasIn && app().LeaveRoom) {
      try { await app().LeaveRoom(); } catch (e) { log.warn('call', 'leaving the room in the core failed', errAttrs(e)); }
    }
    videoStreams.set({});
    callState.update((s) => ({ ...freshState(s), error: s.error }));
  }

  // --- events from the Go core -----------------------------------------------------------

  ensureEvents() {
    if (this.eventsReady || !window.runtime) return;
    this.eventsReady = true;
    EventsOn('call:media_slot', (ev) => this.onSlot(ev));
    EventsOn('call:roster', (r) => this.applyRoster(r));
    EventsOn('call:stream_announced', (st) => this.onStreamAnnounced(st));
    EventsOn('call:stream_closed', ({ streamId }) => { log.info('call', 'stream closed', { streamId }); this.onStreamClosed(streamId); });
    EventsOn('call:active_speaker', ({ speaker, speaking }) => {
      // network VAD is a fallback for peers whose audio we do not play (e.g. deafened)
      if (!get(callState).isDeafened) return;
      callState.update((s) => ({ ...s, speaking: { ...s.speaking, [speaker]: speaking || undefined } }));
    });
    EventsOn('call:kicked', () => {
      log.warn('call', 'removed from the call by the room');
      this.leaveCall().then(() => this.notify(tr('call.notice.removed')));
    });
  }

  async syncFromCore() {
    try {
      const [roster, slots] = await Promise.all([app().GetCallRoster?.(), app().GetCallSlots?.()]);
      if (roster) this.applyRoster(roster);
      (slots || []).forEach((ev) => this.onSlot(ev));
    } catch (e) { log.warn('call', 'could not read the roster and slots from the core', errAttrs(e)); }
  }

  applyRoster(r) {
    if (!r || !r.callId) return;
    const before = get(callState).members.length;
    const after = (r.members || []).length;
    if (before !== after) log.info('call', 'members changed', { from: before, to: after, coordinator: short(r.coordinator) });
    callState.update((s) => ({ ...s, self: r.self || s.self, coordinator: r.coordinator, members: r.members || [] }));
  }

  onSlot(ev) {
    if (!ev?.slot) return;
    log.debug('call', ev.active ? 'playback slot taken' : 'playback slot freed', { slot: ev.slot, kind: ev.kind, peer: short(ev.peer), streamId: ev.streamId });
    callState.update((s) => {
      const slots = { ...s.slots };
      if (ev.active) slots[ev.slot] = { kind: ev.kind, peer: ev.peer, streamId: ev.streamId };
      else delete slots[ev.slot];
      return { ...s, slots };
    });
    if (ev.kind === 'audio' || ev.kind === 'streamaudio') this.applyVolume(ev.slot);
  }

  onStreamAnnounced(st) {
    if (!st?.streamId) return;
    const s = get(callState);
    const known = s.activeStreams.some((x) => x.streamId === st.streamId);
    callState.update((c) => ({
      ...c,
      activeStreams: [...c.activeStreams.filter((x) => x.streamId !== st.streamId), st],
    }));
    const mine = st.broadcaster === s.self;
    if (!known) log.info('call', 'stream announced', { streamId: st.streamId, kind: st.mediaType, by: short(st.broadcaster), quality: st.qualityId, mine });
    if (!known && !mine && st.mediaType === 'camera') {
      const cams = s.watching.filter((id) => s.activeStreams.find((x) => x.streamId === id)?.mediaType === 'camera');
      if (cams.length < AUTO_WATCH_CAMERAS) this.watchStream(st.streamId);
    }
  }

  onStreamClosed(streamId) {
    callState.update((s) => ({
      ...s,
      activeStreams: s.activeStreams.filter((x) => x.streamId !== streamId),
      watching: s.watching.filter((id) => id !== streamId),
      focusedStreamId: s.focusedStreamId === streamId ? null : s.focusedStreamId,
    }));
  }

  // --- remote media ----------------------------------------------------------------------

  slotOf(ev) {
    const sid = ev.streams?.[0]?.id || '';
    if (sid.startsWith('efslot-')) return sid.slice(7);
    const idx = this.pc.getTransceivers().indexOf(ev.transceiver);
    const v0 = LOCAL_LINES + AUDIO_SLOTS;
    const s0 = v0 + VIDEO_SLOTS;
    if (idx >= LOCAL_LINES && idx < v0) return 'a' + (idx - LOCAL_LINES);
    if (idx >= v0 && idx < s0) return 'v' + (idx - v0);
    if (idx >= s0 && idx < s0 + STREAM_AUDIO_SLOTS) return 's' + (idx - s0);
    return '';
  }

  onTrack(ev) {
    const slot = this.slotOf(ev);
    if (!slot) {
      log.warn('call', 'a track arrived on a line with no slot', { kind: ev.track?.kind, stream: ev.streams?.[0]?.id, line: this.pc?.getTransceivers().indexOf(ev.transceiver) });
      return;
    }
    // tracks end by themselves while in the call only when the bridge drops them; leaving ends all
    ev.track.onended = () => { if (this.pc) log.info('call', 'remote track ended', { slot, kind: ev.track.kind }); };
    const stream = ev.streams?.[0] || new MediaStream([ev.track]);
    if (ev.track.kind === 'video') smoothPlayout(ev.receiver);
    if (ev.track.kind === 'audio') {
      this.attachAudio(slot, stream);
    } else {
      videoStreams.update((v) => ({ ...v, [slot]: stream }));
    }
  }

  ensureAudioContext() {
    if (!this.audioCtx) {
      const Ctx = window.AudioContext || window.webkitAudioContext;
      this.audioCtx = new Ctx({ latencyHint: 'interactive', sampleRate: 48000 });
      const out = get(callState).outDeviceId;
      if (out && typeof this.audioCtx.setSinkId === 'function') {
        this.audioCtx.setSinkId(out).catch((e) => log.warn('call', 'the chosen speakers are not available, playing on the default ones', errAttrs(e)));
      }
      this.audioCtx.onstatechange = () => {
        const st = this.audioCtx?.state;
        if (st && st !== 'running') log.warn('call', 'audio output stopped', { state: st });
      };
    }
    if (this.audioCtx.state === 'suspended') this.audioCtx.resume().catch((e) => log.warn('call', 'audio output could not resume', errAttrs(e)));
    return this.audioCtx;
  }

  /** Where remote voices play: an output device id, '' for the system default. */
  async setOutputDevice(deviceId) {
    callState.update((s) => ({ ...s, outDeviceId: deviceId }));
    savePrefs({ out: deviceId });
    const ctx = this.ensureAudioContext();
    if (typeof ctx.setSinkId === 'function') {
      try { await ctx.setSinkId(deviceId || ''); } catch (e) { log.warn('call', 'switching speakers failed', errAttrs(e)); this.notify(String(e?.message || e)); }
    } else {
      this.notify(tr('call.notice.noSink'));
    }
    setSink(deviceId);
  }

  // One graph per speaker: source -> gain (per-person volume, up to 200%) -> analyser -> out.
  attachAudio(slot, stream) {
    const ctx = this.ensureAudioContext();
    const prev = this.audioSlots.get(slot);
    if (prev) { try { prev.source.disconnect(); } catch (_) { /* gone */ } }
    // Chrome only pulls remote WebRTC audio into WebAudio while a media element plays it.
    const el = prev?.el || new Audio();
    el.muted = true;
    el.srcObject = stream;
    el.play().catch((e) => log.warn('call', 'remote audio did not start', errAttrs(e, { slot })));
    const source = ctx.createMediaStreamSource(stream);
    const gain = ctx.createGain();
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 512;
    source.connect(gain).connect(analyser);
    if (!this.muteOutput) analyser.connect(ctx.destination);
    this.audioSlots.set(slot, { el, source, gain, analyser, data: new Float32Array(analyser.fftSize) });
    this.applyVolume(slot);
  }

  applyVolume(slot) {
    const a = this.audioSlots.get(slot);
    if (!a) return;
    const s = get(callState);
    const info = s.slots[slot];
    let v;
    if (slot.startsWith('s')) {
      // a stream's sound has its own volume, apart from its broadcaster's voice
      v = info?.streamId ? (s.streamVolume[info.streamId] ?? 1) : 0;
      if (s.isDeafened) v = 0;
    } else {
      const peer = info?.peer;
      v = peer ? (s.peerVolume[peer] ?? 1) : 1;
      if (s.isDeafened || (peer && s.localMuted[peer])) v = 0;
    }
    a.gain.gain.setTargetAtTime(v, this.audioCtx.currentTime, 0.02);
  }

  applyAllVolumes() {
    for (const slot of this.audioSlots.keys()) this.applyVolume(slot);
  }

  startLevelMonitor() {
    const ctx = this.ensureAudioContext();
    if (this.mic) {
      const an = ctx.createAnalyser();
      an.fftSize = 512;
      ctx.createMediaStreamSource(this.mic).connect(an);
      this.selfAnalyser = { analyser: an, data: new Float32Array(an.fftSize) };
    }
    clearInterval(this.levelTimer);
    this.levelTimer = setInterval(() => this.sampleLevels(), 100);
  }

  sampleLevels() {
    const s = get(callState);
    const speaking = {};
    for (const [slot, a] of this.audioSlots) {
      if (slot.startsWith('s')) continue; // a playing film is not somebody speaking
      const peer = s.slots[slot]?.peer;
      if (!peer || s.localMuted[peer] || s.isDeafened) continue;
      if (rms(a.analyser, a.data) > SPEAKING_LEVEL) speaking[peer] = true;
    }
    if (this.selfAnalyser && s.self) {
      const talking = !s.isMicMuted && rms(this.selfAnalyser.analyser, this.selfAnalyser.data) > SPEAKING_LEVEL;
      if (talking) speaking[s.self] = true;
      if (talking !== this.lastSelfSpeaking) {
        this.lastSelfSpeaking = talking;
        app().SetVADState?.(talking, 0);
      }
    }
    if (s.isDeafened) return; // keep network VAD values
    const prev = s.speaking;
    const same = Object.keys(prev).length === Object.keys(speaking).length && Object.keys(speaking).every((k) => prev[k]);
    if (!same) callState.update((c) => ({ ...c, speaking }));
  }

  // --- local capture ---------------------------------------------------------------------

  async openMic() {
    const { micDeviceId } = get(callState);
    return navigator.mediaDevices.getUserMedia({
      audio: {
        deviceId: micDeviceId ? { exact: micDeviceId } : undefined,
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
      video: false,
    });
  }

  toggleMic() {
    const s = get(callState);
    const track = this.mic?.getAudioTracks()[0];
    if (!track) {
      this.notify(tr('call.notice.noMic'));
      return;
    }
    const muted = !s.isMicMuted;
    track.enabled = !muted;
    // un-muting while deafened also undeafens, like Discord
    const isDeafened = muted ? s.isDeafened : false;
    callState.update((c) => ({ ...c, isMicMuted: muted, isDeafened }));
    if (!isDeafened) this.applyAllVolumes();
    if (muted) app().SetVADState?.(false, 0);
  }

  // Deafen silences everyone and the mic; undeafen restores the mic as it was (Discord-style).
  toggleDeafen() {
    const s = get(callState);
    const deaf = !s.isDeafened;
    let micMuted;
    if (deaf) {
      this.micMutedBeforeDeafen = s.isMicMuted;
      micMuted = true;
    } else {
      micMuted = this.micMutedBeforeDeafen ?? false;
    }
    const track = this.mic?.getAudioTracks()[0];
    if (!track) micMuted = true;
    else track.enabled = !micMuted;
    if (micMuted) app().SetVADState?.(false, 0);
    callState.update((c) => ({ ...c, isDeafened: deaf, isMicMuted: micMuted, speaking: {} }));
    this.applyAllVolumes();
  }

  async setMicDevice(deviceId) {
    callState.update((s) => ({ ...s, micDeviceId: deviceId }));
    savePrefs({ mic: deviceId });
    if (!this.pc) return;
    const next = await this.openMic().catch((e) => {
      log.warn('call', 'the chosen microphone did not open', errAttrs(e));
      this.notify(String(e?.message || e));
      return null;
    });
    if (!next) return;
    const track = next.getAudioTracks()[0];
    track.enabled = !get(callState).isMicMuted;
    try {
      await this.tx.mic.sender.replaceTrack(track);
    } catch (e) {
      log.error('call', 'could not switch the microphone in the call', errAttrs(e));
      next.getTracks().forEach((t) => t.stop());
      return;
    }
    track.onended = () => log.warn('call', 'microphone stopped (unplugged or taken by the system)');
    log.info('call', 'microphone switched', { label: track.label });
    this.mic?.getTracks().forEach((t) => t.stop());
    this.mic = next;
    this.startLevelMonitor();
    callState.update((s) => ({ ...s, hasMic: true }));
  }

  async setCameraDevice(deviceId) {
    callState.update((s) => ({ ...s, camDeviceId: deviceId }));
    savePrefs({ cam: deviceId });
    if (get(callState).isCameraOn) {
      await this.stopCamera();
      await this.startCamera();
    }
  }

  async refreshDevices() {
    try {
      const list = await navigator.mediaDevices.enumerateDevices();
      callState.update((s) => ({
        ...s,
        devices: {
          mics: list.filter((d) => d.kind === 'audioinput').map((d, i) => ({ id: d.deviceId, label: d.label || tr('call.dev.mic', { n: i + 1 }) })),
          cams: list.filter((d) => d.kind === 'videoinput').map((d, i) => ({ id: d.deviceId, label: d.label || tr('call.dev.cam', { n: i + 1 }) })),
          outs: list.filter((d) => d.kind === 'audiooutput').map((d, i) => ({ id: d.deviceId, label: d.label || tr('call.dev.out', { n: i + 1 }) })),
        },
      }));
    } catch (e) { log.warn('call', 'could not list media devices', errAttrs(e)); }
  }

  async loadQualities() {
    try {
      const list = await app().GetStreamQualities?.();
      if (Array.isArray(list) && list.length) callState.update((s) => ({ ...s, qualities: list }));
    } catch (e) { log.warn('call', 'could not read stream qualities, keeping defaults', errAttrs(e)); }
    return get(callState).qualities;
  }

  quality(qid) {
    const s = get(callState);
    return s.qualities.find((q) => q.id === qid) || s.qualities.find((q) => q.fits) || s.qualities[0] ||
      { id: '1080p30', width: 1920, height: 1080, fps: 30, bitrateKbps: 5000 };
  }

  async toggleScreenShare(qualityId = null) {
    if (get(callState).isScreenSharing) return this.stopScreenShare();
    return this.startScreenShare(qualityId);
  }

  async startScreenShare(qualityId = null) {
    if (!this.pc) return;
    const q = this.quality(qualityId || get(callState).selectedQualityId);
    const streamId = 'screen_' + Date.now();
    callState.update((s) => ({ ...s, streamError: null }));
    log.info('screen', 'starting to share the screen', { streamId, quality: q.id });
    try {
      await app().StartStream(streamId, 'screen', q.id); // the core checks upload capacity first
    } catch (err) {
      log.warn('screen', 'the core refused the stream', errAttrs(err, { streamId, quality: q.id }));
      callState.update((s) => ({ ...s, streamError: String(err?.message || err) }));
      return;
    }
    // System audio carries the call itself: the voices this app plays would go back to the
    // viewers, who would hear everyone twice. restrictOwnAudio leaves out what this app plays;
    // where the webview cannot do that, echo cancelling takes the call out of the screen's sound
    // instead (it uses what the app plays as its reference), at some cost to music.
    const canExcludeOwn = !!navigator.mediaDevices.getSupportedConstraints?.().restrictOwnAudio;
    try {
      this.screen = await navigator.mediaDevices.getDisplayMedia({
        video: { width: { ideal: q.width }, height: { ideal: q.height }, frameRate: { ideal: q.fps, max: q.fps } },
        // the sound of the screen: the picker offers it as "share system audio" / a tab's audio
        audio: {
          echoCancellation: !canExcludeOwn, noiseSuppression: false, autoGainControl: false,
          channelCount: { ideal: 2 }, sampleRate: { ideal: 48000 },
          restrictOwnAudio: true, suppressLocalAudioPlayback: false,
        },
        systemAudio: 'include',
        windowAudio: 'system',
      });
    } catch (err) {
      await app().StopScreenShare?.(streamId);
      if (err?.name === 'NotAllowedError') {
        log.info('screen', 'screen picker closed without choosing', { streamId });
      } else {
        log.warn('screen', 'screen capture did not start', errAttrs(err, { streamId }));
        callState.update((s) => ({ ...s, streamError: String(err?.message || err) }));
      }
      return;
    }
    const track = this.screen.getVideoTracks()[0];
    const mode = get(callState).screenMode;
    track.contentHint = mode === 'detail' ? 'detail' : 'motion';
    track.onended = () => {
      log.info('screen', 'screen capture ended (stopped from the system bar or the window closed)', { streamId });
      this.stopScreenShare();
    };
    try {
      await this.tx.screen.sender.replaceTrack(track);
    } catch (err) {
      log.error('screen', 'could not put the screen into the call', errAttrs(err, { streamId }));
      await this.stopScreenShare();
      callState.update((s) => ({ ...s, streamError: String(err?.message || err) }));
      return;
    }
    await applySenderQuality(this.tx.screen.sender, q, mode);
    const vs = track.getSettings?.() || {};
    const sound = this.screen.getAudioTracks()[0] || null;
    let soundGuard = 'none';
    if (sound) {
      sound.contentHint = 'music';
      soundGuard = await this.guardScreenAudio(sound, canExcludeOwn);
      sound.onended = () => log.info('screen', 'screen sound ended', { streamId });
      try {
        await this.tx.screenAudio?.sender.replaceTrack(sound);
      } catch (err) {
        log.error('screen', 'could not put the screen sound into the call', errAttrs(err, { streamId }));
      }
      await applyAudioBitrate(this.tx.screenAudio?.sender, SCREEN_AUDIO_KBPS);
    } else {
      this.notify(tr('call.notice.noScreenAudio'));
    }
    log.info('screen', 'sharing the screen', {
      streamId, surface: vs.displaySurface, width: vs.width, height: vs.height, fps: vs.frameRate, mode,
      sound: !!sound, soundGuard, ...(sound ? audioSettings(sound) : {}),
    });
    videoStreams.update((v) => ({ ...v, 'local:screen': this.screen }));
    callState.update((s) => ({ ...s, isScreenSharing: true, screenStreamId: streamId, streamQualityId: q.id, screenAudio: !!sound }));
  }

  async stopScreenShare() {
    const id = get(callState).screenStreamId;
    this.screen?.getTracks().forEach((t) => { t.onended = null; t.stop(); });
    this.screen = null;
    try { await this.tx?.screen.sender.replaceTrack(null); } catch (_) { /* closed */ }
    try { await this.tx?.screenAudio?.sender.replaceTrack(null); } catch (_) { /* closed */ }
    if (id) {
      log.info('screen', 'stopped sharing the screen', { streamId: id });
      await safeCall('StopScreenShare', id);
    }
    videoStreams.update((v) => { const n = { ...v }; delete n['local:screen']; return n; });
    callState.update((s) => ({ ...s, isScreenSharing: false, screenAudio: false, screenStreamId: null, streamQualityId: null, streamError: null,
      focusedStreamId: s.focusedStreamId === id ? null : s.focusedStreamId }));
  }

  async setStreamQuality(qualityId) {
    callState.update((s) => ({ ...s, selectedQualityId: qualityId }));
    const s = get(callState);
    if (!s.isScreenSharing || !s.screenStreamId) return;
    const q = this.quality(qualityId);
    try {
      await app().UpdateStreamQuality?.(s.screenStreamId, q.id);
    } catch (err) {
      log.warn('screen', 'the core refused the new quality', errAttrs(err, { streamId: s.screenStreamId, quality: q.id }));
      callState.update((c) => ({ ...c, streamError: String(err?.message || err) }));
      return;
    }
    const track = this.screen?.getVideoTracks()[0];
    const mode = get(callState).screenMode;
    if (track) {
      track.contentHint = mode === 'detail' ? 'detail' : 'motion';
      await track.applyConstraints({ width: { ideal: q.width }, height: { ideal: q.height }, frameRate: { ideal: q.fps, max: q.fps } })
        .catch((e) => log.warn('screen', 'the capture did not take the new size or frame rate', errAttrs(e, { quality: q.id })));
      log.info('screen', 'stream quality changed', { streamId: s.screenStreamId, quality: q.id });
    }
    await applySenderQuality(this.tx.screen.sender, q, mode);
    callState.update((c) => ({ ...c, streamQualityId: q.id, streamError: null }));
  }

  /** What a shared screen keeps under pressure: 'motion' (smooth) or 'detail' (sharp); applied at
   *  once to a screen being shared, and remembered. */
  async setScreenMode(mode) {
    mode = mode === 'detail' ? 'detail' : 'motion';
    callState.update((s) => ({ ...s, screenMode: mode }));
    savePrefs({ screenMode: mode });
    const s = get(callState);
    const track = this.screen?.getVideoTracks()[0];
    if (!s.isScreenSharing || !track) return;
    track.contentHint = mode === 'detail' ? 'detail' : 'motion';
    await applySenderQuality(this.tx.screen.sender, this.quality(s.streamQualityId), mode);
    log.info('screen', 'screen mode changed', { streamId: s.screenStreamId, mode });
  }

  /** How our shared screen is encoded now: { encoder, hardware, width, height, fps, kbps, limit }
   *  from the browser's own stats; null when nothing is sent. */
  async screenEncoderInfo() {
    const sender = this.tx?.screen?.sender;
    if (!sender || !this.screen) return null;
    const report = await sender.getStats();
    let info = null;
    report.forEach((r) => {
      if (r.type !== 'outbound-rtp' || r.kind !== 'video') return;
      const before = this.prevEncoderStats;
      info = {
        encoder: r.encoderImplementation || '',
        hardware: r.powerEfficientEncoder === true ? true : r.powerEfficientEncoder === false ? false : null,
        width: r.frameWidth || 0, height: r.frameHeight || 0, fps: Math.round(r.framesPerSecond || 0),
        kbps: rateKbps(r, before), limit: r.qualityLimitationReason && r.qualityLimitationReason !== 'none' ? r.qualityLimitationReason : '',
      };
      this.prevEncoderStats = r;
    });
    return info;
  }

  async toggleCamera() {
    if (get(callState).isCameraOn) return this.stopCamera();
    return this.startCamera();
  }

  async startCamera() {
    if (!this.pc) return;
    const s = get(callState);
    const q = this.quality(s.qualities.find((x) => x.id === '720p30') ? '720p30' : s.selectedQualityId);
    const streamId = 'camera_' + Date.now();
    try {
      await app().StartStream(streamId, 'camera', q.id);
    } catch (err) {
      log.warn('camera', 'the core refused the camera stream', errAttrs(err, { streamId, quality: q.id }));
      callState.update((c) => ({ ...c, streamError: String(err?.message || err) }));
      return;
    }
    try {
      this.camera = await navigator.mediaDevices.getUserMedia({
        video: {
          deviceId: s.camDeviceId ? { exact: s.camDeviceId } : undefined,
          width: { ideal: q.width }, height: { ideal: q.height }, frameRate: { ideal: q.fps, max: q.fps },
        },
        audio: false,
      });
    } catch (err) {
      log.warn('camera', 'camera did not open', errAttrs(err, { streamId, device: s.camDeviceId ? 'chosen' : 'default' }));
      await app().StopScreenShare?.(streamId);
      callState.update((c) => ({ ...c, streamError: tr('call.err.camera') }));
      return;
    }
    const track = this.camera.getVideoTracks()[0];
    track.onended = () => {
      log.info('camera', 'camera stopped (unplugged or taken by another app)', { streamId });
      this.stopCamera();
    };
    try {
      await this.tx.camera.sender.replaceTrack(track);
    } catch (err) {
      log.error('camera', 'could not put the camera into the call', errAttrs(err, { streamId }));
      await this.stopCamera();
      return;
    }
    await applySenderQuality(this.tx.camera.sender, q);
    const cs = track.getSettings?.() || {};
    log.info('camera', 'camera on', { streamId, width: cs.width, height: cs.height, fps: cs.frameRate });
    videoStreams.update((v) => ({ ...v, 'local:camera': this.camera }));
    callState.update((c) => ({ ...c, isCameraOn: true, cameraStreamId: streamId }));
    this.refreshDevices();
  }

  async stopCamera() {
    const id = get(callState).cameraStreamId;
    this.camera?.getTracks().forEach((t) => { t.onended = null; t.stop(); });
    this.camera = null;
    try { await this.tx?.camera.sender.replaceTrack(null); } catch (_) { /* closed */ }
    if (id) {
      log.info('camera', 'camera off', { streamId: id });
      await safeCall('StopScreenShare', id);
    }
    videoStreams.update((v) => { const n = { ...v }; delete n['local:camera']; return n; });
    callState.update((c) => ({ ...c, isCameraOn: false, cameraStreamId: null }));
  }

  // --- watching ----------------------------------------------------------------------------

  async watchStream(streamId, focus = false) {
    const s = get(callState);
    if (!s.watching.includes(streamId)) {
      if (s.watching.length >= VIDEO_SLOTS) {
        this.notify(tr('call.notice.maxStreams', { n: VIDEO_SLOTS }));
        return;
      }
      try {
        await app().WatchStream?.(streamId);
        log.info('call', 'watching a stream', { streamId });
      } catch (err) {
        log.warn('call', 'could not watch the stream', errAttrs(err, { streamId }));
        this.notify(String(err?.message || err));
        return;
      }
    }
    callState.update((c) => ({
      ...c,
      watching: c.watching.includes(streamId) ? c.watching : [...c.watching, streamId],
      focusedStreamId: focus ? streamId : c.focusedStreamId,
    }));
  }

  async unwatchStream(streamId) {
    try { await app().UnwatchStream?.(streamId); } catch (e) { log.debug('call', 'unwatch failed (stream already gone)', errAttrs(e, { streamId })); }
    callState.update((c) => ({
      ...c,
      watching: c.watching.filter((id) => id !== streamId),
      focusedStreamId: c.focusedStreamId === streamId ? null : c.focusedStreamId,
    }));
  }

  focusStream(streamId) {
    callState.update((c) => ({ ...c, focusedStreamId: streamId }));
  }

  /** Remote video MediaStream of a stream, or the local preview of our own. */
  streamFor(streamId, $state, $videos) {
    if (streamId && streamId === $state.screenStreamId) return $videos['local:screen'] || null;
    if (streamId && streamId === $state.cameraStreamId) return $videos['local:camera'] || null;
    const slot = Object.keys($state.slots).find((k) => $state.slots[k].kind === 'video' && $state.slots[k].streamId === streamId);
    return slot ? $videos[slot] || null : null;
  }

  /** Raw totals of one stream for its stats overlay: the browser's side (capture, encoder or
   *  decoder) and the node's (packets sent, delivered, lost, dropped). The overlay diffs two of
   *  them for rates. own: our screen or camera; else the stream we watch, shown by media. */
  async tileStats(streamId, media, own) {
    const out = { at: performance.now(), own, browser: null, node: null };
    try {
      let report = null;
      if (own) {
        const tx = streamId === get(callState).cameraStreamId ? this.tx?.camera : this.tx?.screen;
        report = tx ? await tx.sender.getStats() : null;
      } else {
        const track = media?.getVideoTracks?.()[0];
        const rx = track && this.pc?.getReceivers().find((r) => r.track === track);
        report = rx ? await rx.getStats() : null;
      }
      const b = {};
      report?.forEach((r) => {
        if (r.kind && r.kind !== 'video') return;
        if (r.type === 'media-source') b.captureFps = r.framesPerSecond || 0;
        if (r.type === 'outbound-rtp' || r.type === 'inbound-rtp') Object.assign(b, {
          fps: r.framesPerSecond || 0, width: r.frameWidth || 0, height: r.frameHeight || 0,
          bytes: r.bytesSent ?? r.bytesReceived ?? 0, frames: r.framesSent ?? r.framesReceived ?? 0,
          encoded: r.framesEncoded || 0, decoded: r.framesDecoded || 0, dropped: r.framesDropped || 0,
          packets: r.packetsSent ?? r.packetsReceived ?? 0, lost: r.packetsLost || 0,
          freezes: r.freezeCount || 0, freezeSec: r.totalFreezesDuration || 0, pli: r.pliCount || 0, nack: r.nackCount || 0,
          codecTime: r.totalEncodeTime ?? r.totalDecodeTime ?? 0,
          jbDelay: r.jitterBufferDelay || 0, jbCount: r.jitterBufferEmittedCount || 0,
          impl: r.encoderImplementation || r.decoderImplementation || '',
          hw: r.powerEfficientEncoder ?? r.powerEfficientDecoder ?? null,
          limit: r.qualityLimitationReason && r.qualityLimitationReason !== 'none' ? r.qualityLimitationReason : '',
        });
      });
      out.browser = b;
    } catch (_) { /* the call is closing */ }
    try { out.node = await app().CallStreamStats?.(streamId); } catch (_) { /* an older core, or no call */ }
    return out;
  }

  // --- per-person controls & moderation ----------------------------------------------------

  /** Volume of a watched stream's sound, 0..2 (1 = 100%); 0 mutes it. */
  setStreamVolume(streamId, volume) {
    callState.update((s) => ({ ...s, streamVolume: { ...s.streamVolume, [streamId]: volume } }));
    this.applyAllVolumes();
  }

  /** The sound slot of a watched stream, or '' while none plays. */
  soundSlotOf(streamId, $state) {
    return Object.keys($state.slots).find((k) => $state.slots[k].kind === 'streamaudio' && $state.slots[k].streamId === streamId) || '';
  }

  setPeerVolume(peer, volume) {
    callState.update((s) => ({ ...s, peerVolume: { ...s.peerVolume, [peer]: volume } }));
    this.applyAllVolumes();
  }

  toggleLocalMute(peer) {
    callState.update((s) => {
      const localMuted = { ...s.localMuted };
      if (localMuted[peer]) delete localMuted[peer];
      else localMuted[peer] = true;
      return { ...s, localMuted };
    });
    this.applyAllVolumes();
  }

  async reportPeer(peer, reason = 'spam') {
    // muting for ourselves is immediate; the room removes them once enough members agree
    callState.update((s) => ({ ...s, localMuted: { ...s.localMuted, [peer]: true }, reported: { ...s.reported, [peer]: true } }));
    this.applyAllVolumes();
    try {
      await app().ReportCallMember?.(peer, reason);
      this.notify(tr('call.notice.reported'));
    } catch (err) {
      log.warn('call', 'reporting a member failed', errAttrs(err, { peer: short(peer) }));
      this.notify(String(err?.message || err));
    }
  }

  async switchRelay(streamId) {
    try {
      await app().RequestRelayChange?.(streamId);
      this.notify(tr('call.notice.switching'));
    } catch (err) {
      log.warn('call', 'asking for another relay failed', errAttrs(err, { streamId }));
      this.notify(String(err?.message || err));
    }
  }

  notify(text) {
    clearTimeout(this.noticeTimer);
    callState.update((s) => ({ ...s, notice: text }));
    this.noticeTimer = setTimeout(() => callState.update((s) => ({ ...s, notice: null })), 4000);
  }

  // Go relays never transcode, so every broadcaster sends H264, which every viewer decodes.
  preferH264(transceiver) {
    try {
      const caps = RTCRtpSender.getCapabilities('video');
      if (!caps) return;
      // High profile before Main before Baseline: same bitrate, noticeably more detail
      const rank = (c) => { const p = /profile-level-id=(..)/i.exec(c.sdpFmtpLine || '')?.[1]?.toLowerCase(); return p === '64' ? 0 : p === '4d' ? 1 : 2; };
      const h264 = caps.codecs.filter((c) => c.mimeType === 'video/H264').sort((a, b) => rank(a) - rank(b));
      if (h264.length) transceiver.setCodecPreferences(h264);
      else log.warn('call', 'this webview offers no H264: video will not reach viewers');
    } catch (e) {
      log.warn('call', 'codec preference skipped', errAttrs(e));
    }
  }

  // --- diagnostics -------------------------------------------------------------------------

  // The loopback to the Go bridge should never fail; when it does, nothing reaches anybody.
  watchConnection(pc) {
    pc.onconnectionstatechange = () => {
      const st = pc.connectionState;
      if (st === 'failed' || st === 'disconnected') {
        log.error('call', 'connection to the local media bridge ' + st, { ice: pc.iceConnectionState });
        this.logStats('at bridge ' + st).catch(() => {});
      } else {
        log.info('call', 'media bridge ' + st);
      }
    };
    pc.onicecandidateerror = (ev) => log.warn('call', 'ICE candidate error', { code: ev.errorCode, text: ev.errorText });
  }

  // Once a minute (and when leaving): what the browser sends and receives, so a frozen stream,
  // lost packets or an encoder held back by the CPU show up in the log.
  startStatsLog() {
    clearInterval(this.statsTimer);
    this.prevStats = {};
    this.statsTimer = setInterval(() => this.logStats('periodic').catch(() => {}), STATS_EVERY_MS);
  }

  async logStats(when) {
    const pc = this.pc;
    if (!pc) return;
    const report = await pc.getStats();
    const prev = this.prevStats || {};
    const now = {};
    const out = [];
    const inn = [];
    let lost = 0;
    report.forEach((r) => {
      if (r.type === 'outbound-rtp' && r.bytesSent > 0) {
        now[r.id] = r;
        const limit = r.qualityLimitationReason && r.qualityLimitationReason !== 'none' ? r.qualityLimitationReason : '';
        const enc = r.kind === 'video' && r.encoderImplementation ? ` enc:${r.encoderImplementation}${r.powerEfficientEncoder ? '(hw)' : ''}` : '';
        out.push(`${r.kind}${r.frameWidth ? ` ${r.frameWidth}x${r.frameHeight}@${Math.round(r.framesPerSecond || 0)}` : ''} ${rateKbps(r, prev[r.id])}kbps${enc}${limit ? ' limited:' + limit : ''}`);
        if (limit && prev[r.id]?.qualityLimitationReason !== r.qualityLimitationReason) {
          log.warn('call', 'our ' + r.kind + ' is held back', { by: limit, width: r.frameWidth, height: r.frameHeight, fps: r.framesPerSecond });
        }
      }
      if (r.type === 'inbound-rtp' && r.bytesReceived > 0) {
        now[r.id] = r;
        lost += r.packetsLost || 0;
        inn.push(`${r.kind} ${rateKbps(r, prev[r.id])}kbps lost:${r.packetsLost || 0}` +
          (r.kind === 'video' ? ` ${r.frameWidth || 0}x${r.frameHeight || 0}@${Math.round(r.framesPerSecond || 0)} dropped:${r.framesDropped || 0} freezes:${r.freezeCount || 0}` : ''));
      }
    });
    this.prevStats = now;
    log.info('call', 'media stats ' + when, { state: pc.connectionState, sending: out.join(' | ') || 'nothing', receiving: inn.join(' | ') || 'nothing', lost });
  }

  // The screen's sound must not carry the call back to the viewers. Returns how it is kept out:
  // 'own-audio-excluded', 'echo-cancel', or 'none' when neither worked (the user is told).
  async guardScreenAudio(sound, canExcludeOwn) {
    const st = sound.getSettings?.() || {};
    if (canExcludeOwn && st.restrictOwnAudio !== false) return 'own-audio-excluded';
    if (st.echoCancellation) return 'echo-cancel';
    try {
      await sound.applyConstraints({ echoCancellation: true });
      if (sound.getSettings?.().echoCancellation) {
        log.warn('screen', 'the webview cannot leave its own sound out of the shared audio; echo cancelling it instead');
        return 'echo-cancel';
      }
    } catch (e) {
      log.warn('screen', 'echo cancelling the shared audio failed', errAttrs(e));
    }
    log.warn('screen', 'the call may be heard in the shared audio: neither own-audio exclusion nor echo cancelling is available');
    this.notify(tr('call.notice.screenAudioEcho'));
    return 'none';
  }

  getCurrentState() {
    return get(callState);
  }
}

async function safeCall(method, ...args) {
  try {
    return await app()[method]?.(...args);
  } catch (e) {
    log.warn('call', 'call to the core failed', errAttrs(e, { method }));
    return undefined;
  }
}

const short = (id) => (id ? String(id).slice(0, 12) : '');

function audioSettings(track) {
  const s = track.getSettings?.() || {};
  return { soundChannels: s.channelCount, soundRate: s.sampleRate, ownAudioExcluded: s.restrictOwnAudio, echoCancel: s.echoCancellation };
}

function rateKbps(r, before) {
  const bytes = r.bytesSent ?? r.bytesReceived ?? 0;
  const prevBytes = before ? (before.bytesSent ?? before.bytesReceived ?? 0) : 0;
  const dt = before ? (r.timestamp - before.timestamp) / 1000 : 0;
  return dt > 0 ? Math.round(((bytes - prevBytes) * 8) / dt / 1000) : 0;
}

// Frames that cross relays come unevenly; the browser shows them as they come, so the picture
// stutters even when none is lost. A short buffer before showing them evens that out, at the cost
// of as much delay (the sound waits with them).
const PLAYOUT_MS = 100;
function smoothPlayout(receiver) {
  if (!receiver) return;
  try {
    if ('jitterBufferTarget' in receiver) receiver.jitterBufferTarget = PLAYOUT_MS;
    else if ('playoutDelayHint' in receiver) receiver.playoutDelayHint = PLAYOUT_MS / 1000;
  } catch (e) {
    log.warn('call', 'could not set the playout buffer', errAttrs(e));
  }
}

function rms(analyser, data) {
  analyser.getFloatTimeDomainData(data);
  let sum = 0;
  for (let i = 0; i < data.length; i++) sum += data[i] * data[i];
  return Math.sqrt(sum / data.length);
}

// What gives way when the encoder is short of time or bitrate. A screen in 'detail' mode gives up
// frame rate and keeps its resolution (blurred text is worse than a lower frame rate); in 'motion'
// mode, and a camera at 60 fps, it keeps moving and loses detail instead.
async function applySenderQuality(sender, quality, screenMode = '') {
  if (!sender) return;
  try {
    const params = sender.getParameters();
    if (!params.encodings || !params.encodings.length) params.encodings = [{}];
    params.encodings[0].maxBitrate = quality.bitrateKbps * 1000;
    params.encodings[0].maxFramerate = quality.fps;
    params.encodings[0].scaleResolutionDownBy = 1;
    params.degradationPreference = screenMode === 'detail' ? 'maintain-resolution'
      : screenMode === 'motion' || quality.fps >= 60 ? 'maintain-framerate' : 'balanced';
    await sender.setParameters(params);
  } catch (e) {
    log.warn('call', 'could not set the video bitrate and frame rate', errAttrs(e, { quality: quality.id }));
  }
}

async function applyAudioBitrate(sender, kbps) {
  if (!sender) return;
  try {
    const params = sender.getParameters();
    if (!params.encodings || !params.encodings.length) params.encodings = [{}];
    params.encodings[0].maxBitrate = kbps * 1000;
    params.encodings[0].priority = 'high';
    await sender.setParameters(params);
  } catch (e) {
    log.warn('call', 'could not set the audio bitrate', errAttrs(e, { kbps }));
  }
}

// opusStereo marks Opus as stereo with a music bitrate on the given m-lines (0-based): in an
// offer that makes the browser send stereo, in our answer it makes the browser decode stereo.
export function opusStereo(sdp, lines) {
  const parts = sdp.split(/\r\n(?=m=)/);
  const want = new Set(lines.map((i) => i + 1)); // parts[0] is the session part
  for (let i = 1; i < parts.length; i++) {
    if (!want.has(i)) continue;
    const pt = /a=rtpmap:(\d+) opus\/48000/i.exec(parts[i])?.[1];
    if (!pt) continue;
    parts[i] = parts[i].replace(new RegExp('a=fmtp:' + pt + ' ([^\\r\\n]*)'), (line, params) => {
      let out = params;
      for (const [k, v] of [['stereo', '1'], ['sprop-stereo', '1'], ['maxaveragebitrate', String(SCREEN_AUDIO_KBPS * 1000)]]) {
        if (!new RegExp('(^|;)\\s*' + k + '=').test(out)) out += ';' + k + '=' + v;
      }
      return 'a=fmtp:' + pt + ' ' + out;
    });
  }
  return parts.join('\r\n');
}

// The Go bridge receives one SDP and has no trickle channel, so send the answer with candidates.
function waitIceGathering(pc, timeoutMs) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => { pc.removeEventListener('icegatheringstatechange', check); resolve(); };
    const check = () => pc.iceGatheringState === 'complete' && done();
    pc.addEventListener('icegatheringstatechange', check);
    setTimeout(done, timeoutMs);
  });
}

export const callService = new CallService();
