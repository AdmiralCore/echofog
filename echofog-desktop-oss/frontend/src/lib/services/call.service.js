import { writable, get } from 'svelte/store';
import { EventsOn } from '@wailsjs/runtime/runtime';

// The Go bridge offer has a fixed layout, so tracks are attached with replaceTrack and the
// loopback never renegotiates:
//   m-lines 0..2   UI capture: mic, camera, screen (Go receives)
//   then 16 audio  one playback track per remote speaker  (slots a0..a15)
//   then 4 video   one playback track per watched stream  (slots v0..v3)
const LOCAL = { mic: 0, camera: 1, screen: 2 };
const AUDIO_SLOTS = 16;
const VIDEO_SLOTS = 4;
const SPEAKING_LEVEL = 0.035;
const AUTO_WATCH_CAMERAS = 3;

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
    localMuted: prev.localMuted || {}, // peer -> true: muted for us only
    reported: {},
    screenStreamId: null,
    cameraStreamId: null,
    devices: prev.devices || { mics: [], cams: [] },
    micDeviceId: prev.micDeviceId || '',
    camDeviceId: prev.camDeviceId || '',
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
  }

  // --- lifecycle -------------------------------------------------------------------------

  async joinCall(callId, coordPeerId = '') {
    if (this.pc) return true;
    callState.update((s) => ({ ...freshState(s), connecting: true, callId }));
    this.ensureEvents();
    try {
      this.ensureAudioContext();
      this.mic = await this.openMic().catch(() => null);
      callState.update((s) => ({ ...s, hasMic: !!this.mic, isMicMuted: !this.mic }));

      const pc = new RTCPeerConnection({ iceServers: [] });
      this.pc = pc;
      pc.ontrack = (ev) => this.onTrack(ev);

      if (!app().JoinRoom) throw new Error('Calls are not available in this build');
      const offer = await app().JoinRoom(callId, coordPeerId);
      await pc.setRemoteDescription({ type: 'offer', sdp: offer });

      const t = pc.getTransceivers();
      if (t.length < 3 + AUDIO_SLOTS + VIDEO_SLOTS) throw new Error('Unexpected media bridge layout');
      this.tx = { mic: t[LOCAL.mic], camera: t[LOCAL.camera], screen: t[LOCAL.screen] };
      for (const role of ['mic', 'camera', 'screen']) this.tx[role].direction = 'sendonly';
      this.preferH264(this.tx.camera);
      this.preferH264(this.tx.screen);
      if (this.mic) await this.tx.mic.sender.replaceTrack(this.mic.getAudioTracks()[0]);

      await pc.setLocalDescription(await pc.createAnswer());
      await waitIceGathering(pc, 1500);
      await app().SubmitCallAnswer(pc.localDescription.sdp);

      this.startLevelMonitor();
      callState.update((s) => ({ ...s, inCall: true, connecting: false }));
      this.refreshDevices();
      this.loadQualities();
      this.syncFromCore();
      return true;
    } catch (err) {
      console.error('[CallService] join failed:', err);
      const message = String(err?.message || err);
      await this.leaveCall();
      callState.update((s) => ({ ...s, error: message }));
      throw err;
    }
  }

  async leaveCall() {
    clearInterval(this.levelTimer);
    this.levelTimer = null;
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
      try { await app().LeaveRoom(); } catch (e) { console.warn('[CallService] LeaveRoom:', e); }
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
    EventsOn('call:stream_closed', ({ streamId }) => this.onStreamClosed(streamId));
    EventsOn('call:active_speaker', ({ speaker, speaking }) => {
      // network VAD is a fallback for peers whose audio we do not play (e.g. deafened)
      if (!get(callState).isDeafened) return;
      callState.update((s) => ({ ...s, speaking: { ...s.speaking, [speaker]: speaking || undefined } }));
    });
    EventsOn('call:kicked', () => {
      this.leaveCall().then(() => this.notify('You were removed from the call after reports from other members.'));
    });
  }

  async syncFromCore() {
    try {
      const [roster, slots] = await Promise.all([app().GetCallRoster?.(), app().GetCallSlots?.()]);
      if (roster) this.applyRoster(roster);
      (slots || []).forEach((ev) => this.onSlot(ev));
    } catch (_) { /* older core */ }
  }

  applyRoster(r) {
    if (!r || !r.callId) return;
    callState.update((s) => ({ ...s, self: r.self || s.self, coordinator: r.coordinator, members: r.members || [] }));
  }

  onSlot(ev) {
    if (!ev?.slot) return;
    callState.update((s) => {
      const slots = { ...s.slots };
      if (ev.active) slots[ev.slot] = { kind: ev.kind, peer: ev.peer, streamId: ev.streamId };
      else delete slots[ev.slot];
      return { ...s, slots };
    });
    if (ev.kind === 'audio') this.applyVolume(ev.slot);
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
    if (idx >= 3 && idx < 3 + AUDIO_SLOTS) return 'a' + (idx - 3);
    if (idx >= 3 + AUDIO_SLOTS) return 'v' + (idx - 3 - AUDIO_SLOTS);
    return '';
  }

  onTrack(ev) {
    const slot = this.slotOf(ev);
    if (!slot) return;
    const stream = ev.streams?.[0] || new MediaStream([ev.track]);
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
    }
    if (this.audioCtx.state === 'suspended') this.audioCtx.resume().catch(() => {});
    return this.audioCtx;
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
    el.play().catch(() => {});
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
    const peer = s.slots[slot]?.peer;
    let v = peer ? (s.peerVolume[peer] ?? 1) : 1;
    if (s.isDeafened || (peer && s.localMuted[peer])) v = 0;
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
      this.notify('No microphone available');
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
    if (!this.pc) return;
    const next = await this.openMic().catch((e) => { this.notify(String(e?.message || e)); return null; });
    if (!next) return;
    const track = next.getAudioTracks()[0];
    track.enabled = !get(callState).isMicMuted;
    await this.tx.mic.sender.replaceTrack(track);
    this.mic?.getTracks().forEach((t) => t.stop());
    this.mic = next;
    this.startLevelMonitor();
    callState.update((s) => ({ ...s, hasMic: true }));
  }

  async setCameraDevice(deviceId) {
    callState.update((s) => ({ ...s, camDeviceId: deviceId }));
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
          mics: list.filter((d) => d.kind === 'audioinput').map((d, i) => ({ id: d.deviceId, label: d.label || `Microphone ${i + 1}` })),
          cams: list.filter((d) => d.kind === 'videoinput').map((d, i) => ({ id: d.deviceId, label: d.label || `Camera ${i + 1}` })),
        },
      }));
    } catch (_) { /* no device access */ }
  }

  async loadQualities() {
    try {
      const list = await app().GetStreamQualities?.();
      if (Array.isArray(list) && list.length) callState.update((s) => ({ ...s, qualities: list }));
    } catch (_) { /* keep defaults */ }
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
    try {
      await app().StartStream(streamId, 'screen', q.id); // the core checks upload capacity first
    } catch (err) {
      callState.update((s) => ({ ...s, streamError: String(err?.message || err) }));
      return;
    }
    try {
      this.screen = await navigator.mediaDevices.getDisplayMedia({
        video: { width: { ideal: q.width }, height: { ideal: q.height }, frameRate: { ideal: q.fps, max: q.fps } },
        audio: false,
      });
    } catch (err) {
      await app().StopScreenShare?.(streamId);
      if (err?.name !== 'NotAllowedError') callState.update((s) => ({ ...s, streamError: String(err?.message || err) }));
      return;
    }
    const track = this.screen.getVideoTracks()[0];
    track.contentHint = q.fps >= 60 ? 'motion' : 'detail';
    track.onended = () => this.stopScreenShare();
    await this.tx.screen.sender.replaceTrack(track);
    await applySenderQuality(this.tx.screen.sender, q);
    videoStreams.update((v) => ({ ...v, 'local:screen': this.screen }));
    callState.update((s) => ({ ...s, isScreenSharing: true, screenStreamId: streamId, streamQualityId: q.id }));
  }

  async stopScreenShare() {
    const id = get(callState).screenStreamId;
    this.screen?.getTracks().forEach((t) => { t.onended = null; t.stop(); });
    this.screen = null;
    try { await this.tx?.screen.sender.replaceTrack(null); } catch (_) { /* closed */ }
    if (id) await safeCall('StopScreenShare', id);
    videoStreams.update((v) => { const n = { ...v }; delete n['local:screen']; return n; });
    callState.update((s) => ({ ...s, isScreenSharing: false, screenStreamId: null, streamQualityId: null, streamError: null,
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
      callState.update((c) => ({ ...c, streamError: String(err?.message || err) }));
      return;
    }
    const track = this.screen?.getVideoTracks()[0];
    if (track) {
      track.contentHint = q.fps >= 60 ? 'motion' : 'detail';
      await track.applyConstraints({ width: { ideal: q.width }, height: { ideal: q.height }, frameRate: { ideal: q.fps, max: q.fps } }).catch(() => {});
    }
    await applySenderQuality(this.tx.screen.sender, q);
    callState.update((c) => ({ ...c, streamQualityId: q.id, streamError: null }));
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
      await app().StopScreenShare?.(streamId);
      callState.update((c) => ({ ...c, streamError: 'Camera is not available' }));
      return;
    }
    const track = this.camera.getVideoTracks()[0];
    track.onended = () => this.stopCamera();
    await this.tx.camera.sender.replaceTrack(track);
    await applySenderQuality(this.tx.camera.sender, q);
    videoStreams.update((v) => ({ ...v, 'local:camera': this.camera }));
    callState.update((c) => ({ ...c, isCameraOn: true, cameraStreamId: streamId }));
    this.refreshDevices();
  }

  async stopCamera() {
    const id = get(callState).cameraStreamId;
    this.camera?.getTracks().forEach((t) => { t.onended = null; t.stop(); });
    this.camera = null;
    try { await this.tx?.camera.sender.replaceTrack(null); } catch (_) { /* closed */ }
    if (id) await safeCall('StopScreenShare', id);
    videoStreams.update((v) => { const n = { ...v }; delete n['local:camera']; return n; });
    callState.update((c) => ({ ...c, isCameraOn: false, cameraStreamId: null }));
  }

  // --- watching ----------------------------------------------------------------------------

  async watchStream(streamId, focus = false) {
    const s = get(callState);
    if (!s.watching.includes(streamId)) {
      if (s.watching.length >= VIDEO_SLOTS) {
        this.notify(`You can watch up to ${VIDEO_SLOTS} streams at once`);
        return;
      }
      try {
        await app().WatchStream?.(streamId);
      } catch (err) {
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
    try { await app().UnwatchStream?.(streamId); } catch (_) { /* gone */ }
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

  // --- per-person controls & moderation ----------------------------------------------------

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
      this.notify('Report sent. They are muted for you, and the room removes them if others agree.');
    } catch (err) {
      this.notify(String(err?.message || err));
    }
  }

  async switchRelay(streamId) {
    try {
      await app().RequestRelayChange?.(streamId);
      this.notify('Switching to another relay…');
    } catch (err) {
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
      const h264 = caps.codecs.filter((c) => c.mimeType === 'video/H264');
      if (h264.length) transceiver.setCodecPreferences(h264);
    } catch (e) {
      console.warn('[CallService] codec preference skipped:', e);
    }
  }

  getCurrentState() {
    return get(callState);
  }
}

async function safeCall(method, ...args) {
  try {
    return await app()[method]?.(...args);
  } catch (e) {
    console.warn(`[CallService] ${method}:`, e);
    return undefined;
  }
}

function rms(analyser, data) {
  analyser.getFloatTimeDomainData(data);
  let sum = 0;
  for (let i = 0; i < data.length; i++) sum += data[i] * data[i];
  return Math.sqrt(sum / data.length);
}

async function applySenderQuality(sender, quality) {
  if (!sender) return;
  try {
    const params = sender.getParameters();
    if (!params.encodings || !params.encodings.length) params.encodings = [{}];
    params.encodings[0].maxBitrate = quality.bitrateKbps * 1000;
    params.encodings[0].maxFramerate = quality.fps;
    params.encodings[0].scaleResolutionDownBy = 1;
    params.degradationPreference = quality.fps >= 60 ? 'maintain-framerate' : 'maintain-resolution';
    await sender.setParameters(params);
  } catch (e) {
    console.warn('[CallService] setParameters failed:', e);
  }
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
