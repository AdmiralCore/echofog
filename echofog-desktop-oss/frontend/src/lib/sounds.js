import { writable, get } from 'svelte/store';

// Interface sounds are synthesized: soft sine tones with gentle attacks and long, quiet tails,
// a little bell overtone and a warm low-pass. No files, nothing harsh.

const KEY = 'echofog.sounds';

function load() {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || '{}');
    return { enabled: v.enabled !== false, volume: typeof v.volume === 'number' ? Math.min(1, Math.max(0, v.volume)) : 0.6 };
  } catch (_) {
    return { enabled: true, volume: 0.6 };
  }
}

/** { enabled, volume 0..1 }, remembered between runs */
export const soundSettings = writable(load());
soundSettings.subscribe((v) => {
  try { localStorage.setItem(KEY, JSON.stringify(v)); } catch (_) { /* private mode */ }
});

// note frequencies
const N = { A3: 220, C4: 261.63, E4: 329.63, G4: 392, A4: 440, B4: 493.88, Cs5: 554.37, C5: 523.25, D5: 587.33, E5: 659.25, G5: 783.99, A5: 880, B5: 987.77 };

/**
 * Each sound is a list of notes: { f, at, dur, gain, to (glide target), type, bell (overtone) }.
 * Levels are low on purpose; the master volume comes on top.
 */
const SOUNDS = {
  click: { len: 0.06, notes: [{ f: 1250, to: 950, at: 0, dur: 0.045, gain: 0.07, attack: 0.002 }] },
  send: { len: 0.25, notes: [{ f: N.D5, to: N.A5, at: 0, dur: 0.18, gain: 0.22, glide: 0.07 }] },
  receive: {
    len: 0.9,
    notes: [
      { f: N.E5, at: 0, dur: 0.55, gain: 0.2, bell: true },
      { f: N.B5, at: 0.11, dur: 0.7, gain: 0.17, bell: true },
    ],
  },
  join: {
    len: 0.85,
    notes: [
      { f: N.C5, at: 0, dur: 0.4, gain: 0.17, bell: true },
      { f: N.E5, at: 0.09, dur: 0.4, gain: 0.17, bell: true },
      { f: N.G5, at: 0.18, dur: 0.6, gain: 0.17, bell: true },
    ],
  },
  leave: {
    len: 0.85,
    notes: [
      { f: N.G4, at: 0, dur: 0.4, gain: 0.15, bell: true },
      { f: N.E4, at: 0.1, dur: 0.4, gain: 0.15, bell: true },
      { f: N.C4, at: 0.2, dur: 0.6, gain: 0.16, bell: true },
    ],
  },
  error: {
    len: 0.45,
    notes: [
      { f: N.E4, to: N.C4, at: 0, dur: 0.16, gain: 0.2, type: 'triangle', glide: 0.12 },
      { f: N.C4, to: N.A3, at: 0.15, dur: 0.22, gain: 0.18, type: 'triangle', glide: 0.15 },
    ],
  },
  muteOn: { len: 0.2, notes: [{ f: N.A5, to: N.E5, at: 0, dur: 0.14, gain: 0.14, glide: 0.06 }] },
  muteOff: { len: 0.2, notes: [{ f: N.E5, to: N.A5, at: 0, dur: 0.14, gain: 0.14, glide: 0.06 }] },
  ring: {
    len: 1.6,
    notes: [
      { f: N.A4, at: 0, dur: 0.5, gain: 0.18, bell: true },
      { f: N.Cs5, at: 0.16, dur: 0.5, gain: 0.18, bell: true },
      { f: N.E5, at: 0.32, dur: 0.5, gain: 0.18, bell: true },
      { f: N.A5, at: 0.48, dur: 1.0, gain: 0.15, bell: true },
    ],
  },
};

let ctx = null;
const buffers = new Map();
let lastLoud = 0; // when the last sound other than a click played

function context() {
  if (!ctx) {
    const Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return null;
    ctx = new Ctx();
    try {
      const out = JSON.parse(localStorage.getItem('echofog.devices') || '{}').out;
      if (out && typeof ctx.setSinkId === 'function') ctx.setSinkId(out).catch(() => {});
    } catch (_) { /* no saved choice */ }
  }
  if (ctx.state === 'suspended') ctx.resume().catch(() => {});
  return ctx;
}

function voice(c, out, n) {
  const t0 = n.at;
  const attack = n.attack ?? 0.012;
  const end = t0 + n.dur;
  const partials = n.bell ? [[1, 1], [2, 0.18], [3, 0.05]] : [[1, 1]];
  for (const [mult, level] of partials) {
    const osc = c.createOscillator();
    osc.type = n.type || 'sine';
    osc.frequency.setValueAtTime(n.f * mult, t0);
    if (n.to) osc.frequency.exponentialRampToValueAtTime(n.to * mult, t0 + (n.glide || n.dur * 0.6));
    const g = c.createGain();
    const peak = n.gain * level;
    g.gain.setValueAtTime(0.0001, t0);
    g.gain.linearRampToValueAtTime(peak, t0 + attack);
    // overtones die first, like a struck bell
    g.gain.exponentialRampToValueAtTime(0.0001, mult === 1 ? end : t0 + n.dur / (mult * 1.5));
    osc.connect(g).connect(out);
    osc.start(t0);
    osc.stop(end + 0.05);
  }
}

function render(name) {
  const def = SOUNDS[name];
  const rate = 44100;
  const Offline = window.OfflineAudioContext || window.webkitOfflineAudioContext;
  if (!Offline) return Promise.resolve(null);
  const c = new Offline(1, Math.ceil(rate * (def.len + 0.1)), rate);
  const warm = c.createBiquadFilter();
  warm.type = 'lowpass';
  warm.frequency.value = 3800;
  warm.Q.value = 0.5;
  warm.connect(c.destination);
  def.notes.forEach((n) => voice(c, warm, n));
  return c.startRendering().catch(() => null);
}

/** The rendered sound, for checks and previews. */
export const renderSound = (name) => buffer(name);

function buffer(name) {
  if (!buffers.has(name)) buffers.set(name, render(name));
  return buffers.get(name);
}

/** Plays a named sound. quiet lowers it, e.g. while a call is running. */
export async function play(name, { quiet = false } = {}) {
  const s = get(soundSettings);
  if (!s.enabled || s.volume <= 0 || !SOUNDS[name]) return;
  if (name !== 'click') lastLoud = Date.now();
  const c = context();
  const buf = await buffer(name);
  if (!c || !buf) return;
  const src = c.createBufferSource();
  src.buffer = buf;
  const gain = c.createGain();
  gain.gain.value = s.volume * (quiet ? 0.5 : 1);
  src.connect(gain).connect(c.destination);
  src.start();
}

/** Plays the ring tune every few seconds until the returned function is called. */
export function startRinging() {
  play('ring');
  const timer = setInterval(() => play('ring'), 2600);
  return () => clearInterval(timer);
}

/** Clicks on any enabled button make a small sound, unless the action plays its own. */
export function installClickSounds() {
  const handler = (e) => {
    const btn = e.target instanceof Element ? e.target.closest('button') : null;
    if (!btn || btn.disabled || btn.dataset.silent !== undefined) return;
    const at = Date.now();
    setTimeout(() => { if (lastLoud < at) play('click'); }, 40);
  };
  document.addEventListener('click', handler, true);
  return () => document.removeEventListener('click', handler, true);
}

/** Plays the interface sounds on this output device ('' = system default). */
export async function setSink(deviceId) {
  const c = context();
  if (c && typeof c.setSinkId === 'function') {
    try { await c.setSinkId(deviceId || ''); } catch (_) { /* the device is gone */ }
  }
}

/** Renders the sounds ahead, so the first one has no delay. */
export function preload() {
  Object.keys(SOUNDS).forEach((n) => { buffer(n); });
}
