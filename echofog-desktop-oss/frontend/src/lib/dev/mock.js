// Development-only stand-in for the Wails bridge, enabled with `?mock` in `vite dev`.
// It fakes the Go core well enough to click through chats and calls in a plain browser:
// the "Go side" of the call loopback is a real second RTCPeerConnection with the same
// transceiver layout as the Pion bridge, fed by synthetic voices and canvas video, so the
// UI's slot mapping, per-speaker audio and stream tiles are exercised for real.
import { callService } from '../services/call.service.js';

// deterministic pseudo-random 32-byte IDs, so avatar colours look like real sha256 peer IDs
const hex = (seed) => {
  let x = seed * 2654435761 >>> 0;
  return Array.from({ length: 64 }, () => {
    x ^= x << 13; x >>>= 0; x ^= x >>> 17; x ^= x << 5; x >>>= 0;
    return (x & 15).toString(16);
  }).join('');
};
const SELF = hex(1);
const ALICE = hex(2);
const BOB = hex(3);
const CAROL = hex(4);
const DAVE = hex(5);
const NAMES = { [ALICE]: 'Alice Moreau', [BOB]: 'Bob Ivanov', [CAROL]: 'Carol Chen', [DAVE]: 'Dave Novak' };

const now = Date.now();
const LOG_LINES = [
  ['info', 'Swarm: connected', { peer: 'ab12…9f', addr: '203.0.113.7:4001' }], ['debug', 'DHT: refresh bucket', { bucket: '12' }],
  ['info', 'NAT: classified', { type: 'port-restricted', mapped: 'true' }], ['warn', 'Relay: slow pulse', { ms: '410' }],
  ['debug', 'Call: link pulse', { relay: 'cd34…01' }], ['error', 'Exit probe failed', { domain: 'youtube.com', reason: 'timeout' }],
  ['info', 'Messenger: delivered', { to: 'Alice Moreau' }],
];
const logsStart = Date.now() - 90 * 1000;
function logsMock() {
  const n = Math.floor((Date.now() - logsStart) / 900);
  return Array.from({ length: Math.min(n, 2000) }, (_, i) => {
    const k = n - Math.min(n, 2000) + i;
    const [level, message, attrs] = LOG_LINES[k % LOG_LINES.length];
    return { time: new Date(logsStart + k * 900).toISOString(), level, message: `${message} #${k}`, attrs };
  });
}
const OFFLINE = new URLSearchParams(location.search).has('offline');
const bootMock = { builtIn: OFFLINE ? [{ addr: 'seed1.echofog.example:443', peerId: hex(9), link: '', connected: false }] : [], custom: [] };
const min = 60 * 1000;

// --- event bus (window.runtime) ---------------------------------------------------------------
const listeners = new Map();
const runtimeImpl = {
  EventsOnMultiple(name, cb) {
    if (!listeners.has(name)) listeners.set(name, new Set());
    listeners.get(name).add(cb);
    return () => listeners.get(name)?.delete(cb);
  },
  EventsOff(...names) { names.forEach((n) => listeners.delete(n)); },
  EventsOffAll() { listeners.clear(); },
  EventsEmit(name, ...args) { (listeners.get(name) || []).forEach((cb) => cb(...args)); },
};
const emit = (name, data) => setTimeout(() => runtimeImpl.EventsEmit(name, data), 0);
window.runtime = new Proxy(runtimeImpl, { get: (t, k) => t[k] || (() => undefined) });

// --- fake data ------------------------------------------------------------------------------------
const contacts = [
  { peer_id: ALICE, pub_key: 'pa', alias: NAMES[ALICE], is_online: true },
  { peer_id: BOB, pub_key: 'pb', alias: NAMES[BOB], is_online: true },
  { peer_id: CAROL, pub_key: 'pc', alias: NAMES[CAROL], is_online: false },
];
const sec = (ms) => Math.floor(ms / 1000);
const dm = {
  [ALICE]: [
    [false, 'Hey! Did you see the new relay trees? 4K60 between three of us without a server', now - 26 * 60 * min],
    [true, 'Yes, the 10-node test passed this morning', now - 26 * 60 * min + 2 * min],
    [true, 'Heal time after a relay crash is about a second now', now - 26 * 60 * min + 3 * min],
    [false, 'Nice. Want to jump on a call and I share my screen?', now - 12 * min],
    [false, 'I have the launch checklist open', now - 11 * min],
    [true, 'Sure, calling you in a sec', now - 2 * min, true],
  ],
  [BOB]: [[false, 'Pushed the TON wallet draft, take a look when you can', now - 3 * 60 * min]],
  [CAROL]: [[true, 'See you tomorrow', now - 2 * 24 * 60 * min]],
};
const history = Object.fromEntries(Object.entries(dm).map(([peer, list]) => [peer, list.map(([mine, text, ts, pending], i) => ({
  id: `${peer.slice(0, 4)}-${i}`, text, timestamp: sec(ts), is_mine: mine, sender_id: mine ? SELF : peer, target_id: mine ? peer : SELF, pending: !!pending,
}))]));
// a few more days of Alice: links, files, enough text to search through
history[ALICE].unshift(
  { id: 'al-old-1', text: 'Morning! The relay docs are at https://github.com/echofog/echofog/blob/main/docs/relay.md, and the anti-DPI notes at www.example.org/anti-dpi.', timestamp: sec(now - 3 * 24 * 60 * min), is_mine: false, sender_id: ALICE, target_id: SELF },
  { id: 'al-old-2', text: 'Thanks, reading the relay part now', timestamp: sec(now - 3 * 24 * 60 * min + 4 * min), is_mine: true, sender_id: SELF, target_id: ALICE },
  { id: 'al-old-3', text: 'This one is not a link: javascript:alert(1), neither is file:///C:/Windows', timestamp: sec(now - 3 * 24 * 60 * min + 6 * min), is_mine: false, sender_id: ALICE, target_id: SELF },
  { id: 'al-y-1', text: 'Here are the screenshots from the relay test', timestamp: sec(now - 30 * 60 * min), is_mine: false, sender_id: ALICE, target_id: SELF },
);
// a stranger who wrote first: a message request, not a contact
history[DAVE] = [{ id: 'dv-1', text: 'Hi! Got your key in the EchoFog channel. Could you share your exit with me?', timestamp: sec(now - 40 * min), is_mine: false, sender_id: DAVE, target_id: SELF }];
const privacyMock = {
  privacy: { messages: 'everyone', calls: 'contacts' },
  blocked: [],
  requests: [{ peer_id: DAVE, pub_key: 'pd', first: sec(now - 40 * min), last: sec(now - 40 * min), count: 1 }],
};

const fileMsg = (id, mine, peer, fid, name, size, ts) => ({
  id, type: 4, text: name, timestamp: sec(ts), is_mine: mine, sender_id: mine ? SELF : peer, target_id: mine ? peer : SELF,
  metadata: JSON.stringify({ file_name: name, file_size: size, custom: { fid, root: 'ab'.repeat(32) } }),
});
const MB = 1 << 20;
const fxNamed = {
  shots: { id: 'fx01shots', peer: ALICE, dir: 'in', name: 'relay-test-screenshots.zip', size: 48 * MB + 1234, state: 'done', path: 'C:/Users/demo/Downloads/EchoFog/relay-test-screenshots.zip', done: 48 * MB + 1234, rate: 0, stalled: false, created: now - 29 * 60 * min },
  build: { id: 'fx02build', peer: ALICE, dir: 'in', name: 'EchoFog-rc2-windows.zip', size: 612 * MB, state: 'offered', path: '', done: 0, rate: 0, stalled: false, created: now - 9 * min },
  notes: { id: 'fx03notes', peer: ALICE, dir: 'out', name: 'launch-notes.pdf', size: 3 * MB + 77, state: 'done', path: 'C:/Users/demo/Documents/launch-notes.pdf', done: 3 * MB + 77, rate: 0, stalled: false, created: now - 8 * min },
  video: { id: 'fx04video', peer: BOB, dir: 'out', name: 'demo-call-4k.mp4', size: 1.4 * 1024 * MB, state: 'active', path: 'C:/Users/demo/Videos/demo-call-4k.mp4', done: 0.38 * 1024 * MB, rate: 6.2 * MB, stalled: false, created: now - 2 * 60 * min },
  setup: { id: 'fx05setup', peer: CAROL, dir: 'in', name: 'setup.exe', size: 9 * MB, state: 'done', path: 'C:/Users/demo/Downloads/EchoFog/setup.exe', done: 9 * MB, rate: 0, stalled: false, created: now - 2 * 24 * 60 * min },
};
const fx = Object.fromEntries(Object.values(fxNamed).map((t) => [t.id, t]));
history[ALICE].splice(4, 0, fileMsg('al-f-1', false, ALICE, 'fx01shots', fxNamed.shots.name, fxNamed.shots.size, now - 29 * 60 * min));
history[ALICE].push(
  fileMsg('al-f-2', false, ALICE, 'fx02build', fxNamed.build.name, fxNamed.build.size, now - 9 * min),
  fileMsg('al-f-3', true, ALICE, 'fx03notes', fxNamed.notes.name, fxNamed.notes.size, now - 8 * min),
);
history[ALICE].sort((a, b) => a.timestamp - b.timestamp);
history[BOB].push(fileMsg('bo-f-1', true, BOB, 'fx04video', fxNamed.video.name, fxNamed.video.size, now - 2 * 60 * min));
history[CAROL].unshift(fileMsg('ca-f-1', false, CAROL, 'fx05setup', fxNamed.setup.name, fxNamed.setup.size, now - 2 * 24 * 60 * min));
const pushFx = (t) => { fx[t.id] = t; emit('file:transfer', { ...t }); };
// runs a transfer to the end, a few percent at a time; a peer that "drops" midway stalls for a while
function runFx(t, { stallAt = 0 } = {}) {
  const step = () => {
    if (t.state !== 'active') return;
    if (stallAt && t.done >= t.size * stallAt && !t.stalledOnce) {
      t.stalledOnce = true;
      Object.assign(t, { stalled: true, rate: 0 });
      pushFx(t);
      setTimeout(() => { t.stalled = false; step(); }, 3500);
      return;
    }
    t.rate = Math.round((5 + Math.random() * 4) * MB);
    t.done = Math.min(t.size, t.done + t.rate / 4);
    if (t.done >= t.size) Object.assign(t, { state: 'done', rate: 0, path: t.path || 'C:/Users/demo/Downloads/EchoFog/' + t.name });
    pushFx(t);
    if (t.state === 'active') setTimeout(step, 250);
  };
  setTimeout(step, 250);
}
setTimeout(() => runFx(fxNamed.video), 1500);
const savedMock = [
  { id: `d:${ALICE}/al-old-1`, chatId: 'd:' + ALICE, chatName: NAMES[ALICE], msgId: 'al-old-1', sender: NAMES[ALICE], mine: false, text: history[ALICE][0].text, ts: now - 3 * 24 * 60 * min, savedAt: now - 60 * min },
];
const RISKY = /\.(exe|msi|bat|cmd|com|scr|ps1|vbs|js|jar|lnk|reg|dll|hta|iso)$/i;
function offerFile(pub, path) {
  const c = contacts.find((x) => x.pub_key === pub);
  if (!c) throw new Error('unknown contact');
  const name = String(path).split(/[\\/]/).pop();
  const id = 'fx' + Date.now();
  const t = { id, peer: c.peer_id, dir: 'out', name, size: Math.round((20 + Math.random() * 300) * MB), state: 'offered', path, done: 0, rate: 0, stalled: false, created: Date.now() };
  const msg = fileMsg('m' + id, true, c.peer_id, id, name, t.size, Date.now());
  history[c.peer_id] = [...(history[c.peer_id] || []), msg];
  pushFx(t);
  emit('new_message', msg);
  // the contact accepts after a moment (and, for the demo, briefly drops offline midway)
  setTimeout(() => { t.state = 'active'; pushFx(t); runFx(t, { stallAt: 0.45 }); }, 1800);
  return { ...t };
}

const groupMsgs = [
  [ALICE, 'Release candidate is building, ETA 20 minutes', now - 50 * min],
  [BOB, 'Node runner guide is updated with the Docker one-liner', now - 41 * min],
  [BOB, 'Also added the price-per-GB section', now - 40 * min],
  [SELF, 'Great. Let us do a quick voice sync at 18:00', now - 30 * min],
  [CAROL, 'Works for me', now - 29 * min],
].map(([from, text, ts], i) => ({ id: 'g' + i, sender: from, text, timestamp: ts, isMine: from === SELF, delivered: 4, failed: 0 }));
// files posted in the group: one offer to every member, each pulls it from the sender
const GROUP = 'grp_launch';
const groupFx = {
  deck: { id: 'gfx01deck', peer: ALICE, group: GROUP, dir: 'in', name: 'launch-deck-v3.pdf', size: 18 * MB + 512, state: 'offered', path: '', done: 0, rate: 0, stalled: false, created: now - 35 * min },
  logs: { id: 'gfx02logs', peer: BOB, group: GROUP, dir: 'in', name: 'relay-logs-oct.tar.gz', size: 74 * MB, state: 'done', path: 'C:/Users/demo/Downloads/EchoFog/relay-logs-oct.tar.gz', done: 74 * MB, rate: 0, stalled: false, created: now - 39 * min },
  build: { id: 'gfx03build', peer: '', group: GROUP, dir: 'out', name: 'EchoFog-rc2-setup.exe', size: 96 * MB, state: 'active', path: 'C:/Users/demo/Builds/EchoFog-rc2-setup.exe', done: 41 * MB, rate: 7 * MB, stalled: false, members: 4, delivered: 2, fetching: 1, created: now - 25 * min },
};
Object.values(groupFx).forEach((t) => { fx[t.id] = t; });
const groupFileMsg = (t, from, ts) => ({ id: 'gm-' + t.id, sender: from, text: t.name, timestamp: ts, isMine: from === SELF, delivered: 4, failed: 0, file: { id: t.id, name: t.name, size: t.size } });
groupMsgs.push(groupFileMsg(groupFx.deck, ALICE, now - 35 * min), groupFileMsg(groupFx.logs, BOB, now - 39 * min), groupFileMsg(groupFx.build, SELF, now - 25 * min));
groupMsgs.sort((a, b) => a.timestamp - b.timestamp);
// the member downloading our build finishes; nobody else is fetching then
function runGroupShare(t) {
  const step = () => {
    if (t.state !== 'active' || !t.fetching) return;
    t.done = Math.min(t.size, t.done + t.rate / 4);
    if (t.done >= t.size) Object.assign(t, { delivered: t.delivered + 1, fetching: 0, rate: 0 });
    pushFx(t);
    if (t.fetching) setTimeout(step, 250);
  };
  setTimeout(step, 250);
}
setTimeout(() => runGroupShare(groupFx.build), 2000);
function postGroupFile(groupId, path) {
  const name = String(path).split(/[\/]/).pop();
  const id = 'gfx' + Date.now();
  const t = { id, peer: '', group: groupId, dir: 'out', name, size: Math.round((5 + Math.random() * 80) * MB), state: 'offered', path, done: 0, rate: 0, stalled: false, members: groupMembers.length - 1, delivered: 0, fetching: 0, created: Date.now() };
  const m = groupFileMsg(t, SELF, Date.now());
  groupMsgs.push(m);
  pushFx(t);
  // two members start downloading after a moment
  setTimeout(() => { Object.assign(t, { state: 'active', fetching: 2, rate: 6 * MB }); pushFx(t); runGroupShare(t); }, 1500);
  return { ...m };
}

const qualities = [
  ['360p30', '360p 30 fps', 640, 360, 30, 800], ['720p30', '720p 30 fps', 1280, 720, 30, 2500], ['720p60', '720p 60 fps', 1280, 720, 60, 4000],
  ['1080p30', '1080p 30 fps', 1920, 1080, 30, 5000], ['1080p60', '1080p 60 fps', 1920, 1080, 60, 8000], ['1440p60', '1440p 60 fps', 2560, 1440, 60, 14000],
  ['2160p30', '4K 30 fps', 3840, 2160, 30, 16000], ['2160p60', '4K 60 fps', 3840, 2160, 60, 25000],
].map(([id, label, width, height, fps, bitrateKbps]) => ({ id, label, width, height, fps, bitrateKbps, codec: 'h264', fits: bitrateKbps <= 10000 }));

// --- fake call core ---------------------------------------------------------------------------------
const call = { pc: null, members: [], slots: {}, streams: [], painters: [], voices: [], audioCtx: null, timer: null };
const scenes = {
  'alice-screen': { owner: ALICE, kind: 'screen', title: 'launch-checklist.md' },
  'bob-cam': { owner: BOB, kind: 'camera', title: 'Bob' },
};

function painter(canvas) {
  const g = canvas.getContext('2d');
  let scene = null;
  let t = 0;
  const draw = () => {
    t += 1;
    const { width: w, height: h } = canvas;
    if (!scene) {
      g.fillStyle = '#000';
      g.fillRect(0, 0, w, h);
    } else if (scene.kind === 'screen') {
      g.fillStyle = '#1e1f22';
      g.fillRect(0, 0, w, h);
      g.fillStyle = '#2b2d31';
      g.fillRect(0, 0, w, 44);
      g.fillStyle = '#dbdee1';
      g.font = '600 20px Inter, sans-serif';
      g.fillText(scene.title, 24, 29);
      const lines = ['## Launch checklist', '- [x] Relay trees: 4K60, heal < 1 s', '- [x] Race detector clean', '- [x] Per-speaker audio tracks',
        '- [ ] TON wallet top-up flow', '- [ ] VPN market UI', '- [ ] Store listing'];
      g.font = '22px "JetBrains Mono", monospace';
      lines.forEach((l, i) => {
        g.fillStyle = l.startsWith('##') ? '#5ea1e0' : l.includes('[x]') ? '#7bc862' : '#b5bac1';
        g.fillText(l, 48, 110 + i * 42);
      });
      const cy = 110 + ((Math.floor(t / 30) % lines.length) * 42);
      g.fillStyle = 'rgba(94,161,224,0.15)';
      g.fillRect(36, cy - 28, w - 72, 38);
      g.fillStyle = '#fff';
      g.beginPath();
      g.arc(w * 0.7 + Math.sin(t / 40) * 120, h * 0.72 + Math.cos(t / 33) * 60, 8, 0, Math.PI * 2);
      g.fill();
    } else {
      const grd = g.createLinearGradient(0, 0, w, h);
      grd.addColorStop(0, '#3b2f63');
      grd.addColorStop(1, '#0f3443');
      g.fillStyle = grd;
      g.fillRect(0, 0, w, h);
      g.fillStyle = '#f2c9a0';
      g.beginPath();
      g.arc(w / 2 + Math.sin(t / 50) * 10, h / 2 + 10, h * 0.22, 0, Math.PI * 2);
      g.fill();
      g.fillStyle = '#2b2d31';
      g.fillRect(w / 2 - h * 0.35, h * 0.78, h * 0.7, h * 0.3);
    }
  };
  // setInterval, not requestAnimationFrame: rAF stops in hidden windows and freezes the stream
  setInterval(draw, 33);
  return { set(s) { scene = s; } };
}

function voice(ctx, i) {
  const dest = ctx.createMediaStreamDestination();
  const osc = ctx.createOscillator();
  const formant = ctx.createBiquadFilter();
  const gain = ctx.createGain();
  osc.type = 'sawtooth';
  osc.frequency.value = 110 + i * 37;
  formant.type = 'bandpass';
  formant.frequency.value = 900;
  gain.gain.value = 0;
  osc.connect(formant).connect(gain).connect(dest);
  osc.start();
  return { dest, gain, osc };
}

async function joinRoom() {
  const ctx = new AudioContext();
  call.audioCtx = ctx;
  const pc = new RTCPeerConnection();
  call.pc = pc;
  pc.addTransceiver('audio', { direction: 'recvonly' }); // mic
  pc.addTransceiver('video', { direction: 'recvonly' }); // camera
  pc.addTransceiver('video', { direction: 'recvonly' }); // screen
  pc.addTransceiver('audio', { direction: 'recvonly' }); // screen sound
  for (let i = 0; i < 16; i++) {
    const v = voice(ctx, i);
    call.voices.push(v);
    pc.addTransceiver(v.dest.stream.getAudioTracks()[0], { direction: 'sendonly', streams: [v.dest.stream] });
  }
  for (let i = 0; i < 4; i++) {
    const c = document.createElement('canvas');
    c.width = 1280;
    c.height = 720;
    call.painters.push(painter(c));
    const s = c.captureStream(30);
    pc.addTransceiver(s.getVideoTracks()[0], { direction: 'sendonly', streams: [s] });
  }
  // the sound of watched streams (s0..s3): a soft chord, so a muted stream is easy to tell apart
  for (let i = 0; i < 4; i++) {
    const v = voice(ctx, 20 + i);
    v.gain.gain.value = 0.08;
    pc.addTransceiver(v.dest.stream.getAudioTracks()[0], { direction: 'sendonly', streams: [v.dest.stream] });
  }
  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await new Promise((r) => (pc.iceGatheringState === 'complete' ? r() : pc.addEventListener('icegatheringstatechange', () => pc.iceGatheringState === 'complete' && r())));
  return pc.localDescription.sdp;
}

function roster() {
  return { callId: 'room_demo', self: SELF, coordinator: SELF, members: call.members, streams: call.streams.map((s) => s.streamId) };
}

async function submitAnswer(sdp) {
  await call.pc.setRemoteDescription({ type: 'answer', sdp });
  call.members = [SELF, ALICE, BOB, CAROL];
  emit('call:roster', roster());
  [ALICE, BOB, CAROL].forEach((peer, i) => {
    call.slots['a' + i] = { kind: 'audio', peer, streamId: 'voice' };
    emit('call:media_slot', { slot: 'a' + i, kind: 'audio', peer, streamId: 'voice', active: true });
  });
  setTimeout(() => announce('bob-cam', 'camera', 1280, 720, 30, '720p30'), 600);
  setTimeout(() => announce('alice-screen', 'screen', 2560, 1440, 60, '1440p60'), 1400);
  setTimeout(() => {
    call.members = [...call.members, DAVE];
    emit('call:roster', roster());
  }, 2500);
  // people take turns talking
  let turn = 0;
  call.timer = setInterval(() => {
    turn = (turn + 1) % 6;
    call.voices.forEach((v, i) => v.gain.gain.setTargetAtTime(i === turn % 3 && turn < 5 ? 0.25 : 0, call.audioCtx.currentTime, 0.05));
  }, 1600);
}

function announce(streamId, mediaType, width, height, fps, qualityId) {
  const st = { streamId, broadcaster: scenes[streamId].owner, mediaType, bitrate: 0, width, height, fps, codec: 'h264', qualityId };
  call.streams.push(st);
  emit('call:stream_announced', st);
}

function watch(streamId) {
  const used = Object.keys(call.slots).filter((k) => k.startsWith('v'));
  const slot = 'v' + [0, 1, 2, 3].find((i) => !used.includes('v' + i));
  call.slots[slot] = { kind: 'video', peer: scenes[streamId]?.owner, streamId };
  call.painters[Number(slot.slice(1))].set(scenes[streamId] || null);
  setTimeout(() => emit('call:media_slot', { slot, kind: 'video', peer: scenes[streamId]?.owner, streamId, active: true }), 300);
  // screens come with their sound, on the stream sound slot of the same number
  if (scenes[streamId]?.kind === 'screen') {
    const s = 's' + slot.slice(1);
    call.slots[s] = { kind: 'streamaudio', peer: scenes[streamId]?.owner, streamId };
    setTimeout(() => emit('call:media_slot', { slot: s, kind: 'streamaudio', peer: scenes[streamId]?.owner, streamId, active: true }), 300);
  }
}

function unwatch(streamId) {
  for (const slot of Object.keys(call.slots).filter((k) => call.slots[k].streamId === streamId && (k.startsWith('v') || k.startsWith('s')))) {
    const kind = call.slots[slot].kind;
    delete call.slots[slot];
    emit('call:media_slot', { slot, kind, peer: '', streamId, active: false });
  }
}

function leave() {
  clearInterval(call.timer);
  call.pc?.close();
  call.audioCtx?.close();
  Object.assign(call, { pc: null, members: [], slots: {}, streams: [], painters: [], voices: [], audioCtx: null, timer: null });
}

// --- App bindings -----------------------------------------------------------------------------------
const impl = {
  GetProfiles: () => ['demo'],
  AppVersion: () => '0.1.0',
  CheckForUpdate: () => ({ current: '0.1.0', latest: '0.1.1', url: 'https://github.com/AdmiralCore/EchoFog/releases/tag/v0.1.1', newer: true }),
  SelectProfile: () => null,
  // ?mock&fresh walks the first-run path: create, recovery phrase, access choice
  IsRegistered: () => !new URLSearchParams(location.search).has('fresh'),
  // the app password to delete a profile in the preview is "demo"; anything else is refused
  DeleteProfile: (_name, pw) => { if (pw !== 'demo') throw new Error('wrong app password'); return null; },
  Login: () => null,
  Register: () => 'abandon ability able about above absent absorb abstract absurd abuse access accident',
  CheckRecoveryPhrase: (p) => {
    const w = p.trim().toLowerCase().split(/\s+/).filter(Boolean);
    if (w.length !== 12 && w.length !== 24) return `a recovery phrase has 12 or 24 words, this one has ${w.length}`;
    const bad = w.findIndex((x) => !/^[a-z]{3,8}$/.test(x));
    return bad >= 0 ? `word ${bad + 1} ("${w[bad]}") is not in the recovery word list: check its spelling` : '';
  },
  StartNode: () => null,
  GetMyID: () => SELF,
  GetMyPubKey: () => hex(11),
  Logout: () => null,
  // ?offline: no peers, to see the offline banner and the bootstrap card
  GetNetworkStatus: () => (OFFLINE && !bootMock.custom.some((n) => n.connected) ? { peerCount: 0, bytesRx: 0, bytesTx: 0, status: 'OFFLINE' } : { peerCount: 7, bytesRx: 0, bytesTx: 0, status: 'ONLINE' }),
  // logs: a new line every 0.9 s, levels mixed
  GetLogs: () => logsMock(),
  GetLogsText: () => logsMock().map((e) => `${e.time} ${e.level.toUpperCase()} ${e.message}`).join('\n'),
  ExportLogs: () => 'C:/Users/demo/echofog-logs-mock.txt',
  GetBootstrapNodes: () => ({ ...bootMock, peers: OFFLINE && !bootMock.custom.some((n) => n.connected) ? 0 : 7 }),
  AddBootstrapNode: (entry) => {
    const m = /^(?:echofog:\/\/relay\/)?(.+:\d+)\/([0-9a-f]{64})\/?$/i.exec(entry.trim());
    if (!m) throw new Error('a relay address needs the peer ID at the end: host:port/<64 hex characters>');
    const peerId = m[2].toLowerCase();
    if (!bootMock.custom.some((n) => n.peerId === peerId)) {
      const node = { addr: m[1], peerId, link: 'echofog://relay/' + m[1] + '/' + peerId, connected: false };
      bootMock.custom.push(node);
      setTimeout(() => { node.connected = true; }, 2500);
    }
    return null;
  },
  RemoveBootstrapNode: (entry) => { bootMock.custom = bootMock.custom.filter((n) => !entry.toLowerCase().endsWith(n.peerId)); return null; },
  GetSettings: () => ({ ...settingsMock }),
  SetSettings: (v) => { if (v.socketBufferMb < 1 || v.socketBufferMb > 256) throw new Error('socket buffer must be between 1 and 256 MB'); Object.assign(settingsMock, v); return null; },
  AddContactByKey: (key, alias) => {
    if (!/^[0-9a-f]{64}$/i.test(key.trim())) throw new Error('a contact key is 64 hexadecimal characters');
    const c = { peer_id: hex(contacts.length + 20), pub_key: key.trim(), alias, is_online: false };
    contacts.push(c);
    return c;
  },
  DeleteContact: (id) => { contacts.splice(contacts.findIndex((c) => c.peer_id === id), 1); return null; },
  GetSharingStatus: () => ({ ...sharingMock }),
  StartSharing: (cfg) => { Object.assign(sharingMock, { enabled: true, config: { ...cfg, freePeers: cfg.freePeers || sharingMock.config.freePeers }, lastPublish: sec(Date.now()) }); return null; },
  SetExitFreePeers: (ids) => { if (ids.some((x) => !HEX64.test(x))) throw new Error('not a peer ID (64 hex characters)'); sharingMock.config.freePeers = [...ids]; return null; },
  // relays: what this computer does for others, its link, and the relays it uses (?nat=1 shows the no-link case)
  GetRelayStatus: () => relayStatus(),
  SetRelaySettings: (r) => {
    if ((r.allowedPeers || []).some((x) => !HEX64.test(x))) throw new Error('not a peer ID (64 hex characters)');
    if (r.listenPort < 0 || r.listenPort > 65535) throw new Error('the listen port must be between 1 and 65535, or 0 for any free port');
    Object.assign(relayMock, { settings: { ...r, server: r.server || r.signalingOnly }, chosen: true });
    return null;
  },
  AddRelay: (link) => {
    const m = /^(?:echofog:\/\/relay\/)?(.+:\d+)\/([0-9a-f]{64})\/?$/i.exec(link.trim());
    if (!m) throw new Error('a relay address needs the peer ID at the end: host:port/<64 hex characters>');
    const addr = m[1] + '/' + m[2].toLowerCase();
    if (!relayMock.settings.relays.includes(addr)) relayMock.settings.relays.push(addr);
    relayMock.connectedAt[m[2].toLowerCase()] = Date.now() + 2500;
    return null;
  },
  // access: public or hidden, the invitation link, who joined with it
  GetAccessStatus: () => ({ ...accessMock, link: accessLink(), joined: accessMock.joined.map((j) => ({ ...j })) }),
  SetAccessMode: (mode) => {
    if (mode !== 'public' && mode !== 'hidden') throw new Error('access mode must be public or hidden');
    accessMock.nextMode = mode === accessMock.mode ? '' : mode;
    return null;
  },
  RotateAccessSecret: () => { accessMock.secret = randomSecret(); return accessLink(); },
  RemoveJoinedPeer: (id) => { accessMock.joined = accessMock.joined.filter((j) => j.peerId !== id); return null; },
  ParseInvite: (link) => {
    const q = new URLSearchParams(link.trim().replace(/^echofog:\/\/join\?/i, ''));
    if (Number(q.get('v')) > 1) throw new Error('the link was made by a newer version of the app: update it');
    if (q.get('net') !== 'echofog') throw new Error(`the link is for the network "${q.get('net')}", this app is for another one`);
    if (!HEX64.test(q.get('id') || '')) throw new Error('broken invitation link: bad node id');
    return { peerId: q.get('id').toLowerCase(), private: !!q.get('s'), addrs: q.getAll('a').length, relays: q.getAll('r').length, protocol: Number(q.get('p')) || 0 };
  },
  StartVPNVia: (target) => new Promise((r, j) => setTimeout(() => {
    const m = /^echofog:\/\/join\?/i.test(target.trim()) ? /[?&]id=([0-9a-f]{64})/i.exec(target) : /([0-9a-f]{64})\/?$/i.exec(target.trim());
    if (!m) return j(new Error('a relay address needs the peer ID at the end: host:port/<64 hex characters>'));
    Object.assign(vpnMock, { running: true, listenAddr: '127.0.0.1:1080', tunnelReady: true, tunnelNotice: '', exitState: 'ok', relayPeerId: m[1].toLowerCase() });
    emit('vpn:started', {});
    r(null);
  }, 700)),
  StopSharing: () => { sharingMock.enabled = false; return null; },
  MeasureUploadCapacity: () => new Promise((r) => setTimeout(() => r(38400), 700)),
  RefreshExitNodes: () => new Promise((r) => setTimeout(() => r(2), 400)),
  ProbeExitNode: () => new Promise((r) => setTimeout(() => r({ ok: true, latencyMs: 41, measuredKbps: 39000, domainsTried: 3, domainsOk: 3, paymentRequired: false }), 600)),
  GetProbeDomains: () => [...probeDomains],
  SetProbeDomains: (d) => { probeDomains = [...d]; return null; },
  // directly the usual suspects are blocked, through the exit everything opens but one
  CheckSites: (exit, sites) => new Promise((r) => setTimeout(() => r(sites.map((s) => {
    const host = s.replace(/^https?:\/\//, '').split('/')[0];
    const target = host.includes(':') ? host : host + ':443';
    const blocked = /youtube|discord|instagram|facebook|x\.com|signal|linkedin|viber|whatsapp|chatgpt/.test(target);
    const ms = 40 + Math.round(Math.random() * 160);
    return {
      host: target,
      direct: blocked ? { ok: false, ms: 0, reason: /youtube/.test(target) ? 'no answer' : 'connection reset (blocked)' } : { ok: true, ms },
      exit: exit === '' ? undefined : /spotify/.test(target) ? { ok: false, ms: 0, reason: 'the exit could not reach it' } : { ok: true, ms: ms + 60 },
    };
  })), 900)),
  GetGroupInvite: () => 'fogroup1:demo-invite-0f3a9c1e',
  GetContacts: () => contacts,
  GetPrivacy: () => ({ ...privacyMock.privacy }),
  SetPrivacy: (p) => {
    if (!['everyone', 'contacts'].includes(p.messages) || !['everyone', 'contacts', 'nobody'].includes(p.calls)) throw new Error('messages: everyone or contacts; calls: everyone, contacts or nobody');
    privacyMock.privacy = { messages: p.messages, calls: p.calls };
    return null;
  },
  GetBlockedPeers: () => privacyMock.blocked.map((b) => ({ ...b })),
  BlockPeer: (id) => {
    const alias = contacts.find((c) => c.peer_id === id)?.alias || '';
    privacyMock.blocked = [{ peer_id: id, alias, at: sec(Date.now()) }, ...privacyMock.blocked.filter((b) => b.peer_id !== id)];
    privacyMock.requests = privacyMock.requests.filter((r) => r.peer_id !== id);
    return null;
  },
  UnblockPeer: (id) => { privacyMock.blocked = privacyMock.blocked.filter((b) => b.peer_id !== id); return null; },
  GetMessageRequests: () => privacyMock.requests.map((r) => ({ ...r })),
  AcceptMessageRequest: (id, alias) => {
    const r = privacyMock.requests.find((x) => x.peer_id === id);
    if (!r) throw new Error('no request from this peer');
    contacts.push({ peer_id: id, pub_key: r.pub_key, alias: alias || 'Unknown User', is_online: true });
    privacyMock.requests = privacyMock.requests.filter((x) => x.peer_id !== id);
    return null;
  },
  DismissMessageRequest: (id) => { privacyMock.requests = privacyMock.requests.filter((x) => x.peer_id !== id); return null; },
  GetChatHistory: (peer) => [...(history[peer] || [])].reverse(),
  SendMessage: (pub, text) => {
    const c = contacts.find((x) => x.pub_key === pub);
    const msg = { id: 'm' + Date.now(), text, timestamp: sec(Date.now()), is_mine: true, sender_id: SELF, target_id: c.peer_id };
    history[c.peer_id] = [...(history[c.peer_id] || []), msg];
    return null;
  },
  // files: see fx above
  ListFileTransfers: (peer) => Object.values(fx).filter((t) => !peer || t.peer === peer).map((t) => ({ ...t })),
  PickAndSendFile: (pub) => new Promise((r) => setTimeout(() => r(offerFile(pub, 'C:/Users/demo/Pictures/holiday-photos.7z')), 300)),
  SendFile: (pub, path) => offerFile(pub, path),
  PickAndSendGroupFile: (g) => new Promise((r) => setTimeout(() => r(postGroupFile(g, 'C:/Users/demo/Documents/roadmap-q4.xlsx')), 300)),
  SendGroupFile: (g, path) => postGroupFile(g, path),
  AcceptFile: (id) => {
    const t = fx[id];
    if (!t || t.state !== 'offered') throw new Error('the transfer is ' + (t ? t.state : 'gone'));
    if (t.size > 600 * MB && !t.warned) { t.warned = true; throw new Error('not enough disk space: 612.0 MiB needed, 410.3 MiB free (try again: the preview frees some)'); }
    Object.assign(t, { state: 'active', path: 'C:/Users/demo/Downloads/EchoFog/' + t.name });
    pushFx(t);
    runFx(t);
    return { ...t };
  },
  DeclineFile: (id) => { Object.assign(fx[id], { state: 'declined' }); pushFx(fx[id]); return null; },
  CancelFile: (id) => { Object.assign(fx[id], { state: 'cancelled', rate: 0 }); pushFx(fx[id]); return null; },
  IsRiskyFile: (name) => RISKY.test(name),
  OpenTransferFile: (id, allowRisky) => { if (RISKY.test(fx[id].name) && !allowRisky) throw new Error('this file runs a program when opened'); console.info('[mock] open', fx[id].path); return null; },
  ShowTransferFile: (id) => { console.info('[mock] show in folder', fx[id].path); return null; },
  GetDownloadDir: () => 'C:/Users/demo/Downloads/EchoFog',
  // saved messages
  ListSavedMessages: () => [...savedMock].sort((a, b) => b.savedAt - a.savedAt),
  SaveMessage: (m) => {
    const s = { ...m, id: m.chatId + '/' + m.msgId, savedAt: Date.now() };
    const i = savedMock.findIndex((x) => x.id === s.id);
    if (i >= 0) savedMock.splice(i, 1);
    savedMock.push(s);
    return s;
  },
  UnsaveMessage: (id) => { const i = savedMock.findIndex((x) => x.id === id); if (i >= 0) savedMock.splice(i, 1); return null; },
  // the preview has no save dialog: the browser downloads the text
  SaveTextFile: (name, text) => {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    return name;
  },
  GetPeerBalance: () => 0,
  ListGroups: () => groups,
  CreateGroup: (title, _d, kind) => { const g = { id: 'grp_' + Date.now(), title, kind, members: 1, myRole: 'owner', readOnly: kind === 'channel', description: '' }; groups.push(g); return g; },
  JoinGroup: (invite) => { if (!invite.startsWith('fogroup1:')) throw new Error('this is not a group invite'); return groups[0]; },
  GetGroupMessages: () => groupMsgs,
  SendGroupMessage: (id, text) => {
    const m = { id: 'g' + Date.now(), sender: SELF, text, timestamp: Date.now(), isMine: true, delivered: 4, failed: 0 };
    groupMsgs.push(m);
    return m;
  },
  ListGroupMembers: () => groupMembers.map((m) => ({ ...m, isMe: m.peerId === SELF })),
  SetGroupRole: (_g, id, role) => { const m = groupMembers.find((x) => x.peerId === id); if (!m) throw new Error('this person is not in the group'); m.role = role; return null; },
  RemoveGroupMember: (_g, id) => { const i = groupMembers.findIndex((x) => x.peerId === id); if (i < 0) throw new Error('this person is not in the group'); groupMembers.splice(i, 1); return null; },
  AddGroupMember: (_g, key) => { if (!/^[0-9a-f]{64}$/i.test(key)) throw new Error('member key must be 32 bytes of hex'); groupMembers.push({ peerId: hex(40 + groupMembers.length), role: 'member' }); return null; },
  LeaveGroup: () => null,
  TransferGroupOwnership: (_g, id) => { groupMembers.forEach((m) => { if (m.role === 'owner') m.role = 'admin'; }); const t = groupMembers.find((x) => x.peerId === id); t.role = 'owner'; groups[0].myRole = 'admin'; return null; },
  DeleteGroup: () => { groups.length = 0; return null; },
  EndGroupCall: () => null,
  StartGroupCall: () => ({ callId: 'call_group_demo', coordinator: SELF, from: SELF, startedAt: Math.floor(Date.now() / 1000) }),
  ActiveGroupCall: () => null,
  KeepGroupCallAlive: () => null,
  InviteToCall: () => ({ callId: 'call_demo', coordinator: SELF }),
  GetStreamQualities: () => qualities,
  SetUploadCapacity: () => null,
  JoinRoom: () => joinRoom(),
  SubmitCallAnswer: (sdp) => submitAnswer(sdp),
  GetCallRoster: () => roster(),
  GetCallSlots: () => Object.entries(call.slots).map(([slot, v]) => ({ slot, ...v, active: true })),
  WatchStream: (id) => watch(id),
  UnwatchStream: (id) => unwatch(id),
  StartStream: (id, type, q) => {
    if (q === '2160p60') throw new Error('4K 60 fps needs 25000 kbps, available 20000 kbps');
    const qq = qualities.find((x) => x.id === q) || qualities[3];
    // like the core: our own stream is announced to our UI too
    emit('call:stream_announced', { streamId: id, broadcaster: SELF, mediaType: type, bitrate: qq.bitrateKbps, width: qq.width, height: qq.height, fps: qq.fps, codec: 'h264', qualityId: qq.id });
    return null;
  },
  StopScreenShare: (id) => { emit('call:stream_closed', { streamId: id }); return null; },
  UpdateStreamQuality: () => null,
  SetVADState: () => null,
  LeaveRoom: () => leave(),
  ReportCallMember: (peer) => {
    setTimeout(() => {
      call.members = call.members.filter((p) => p !== peer);
      emit('call:roster', roster());
    }, 1500);
    return null;
  },
  RequestRelayChange: () => null,
  GetTonWallet: () => ({ address: 'UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAJKZ', rawAddress: '0:0000…', network: 'mainnet', topUpLink: 'ton://transfer/UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAJKZ', frozen: false }),
  GetTonBalance: () => ({ nano: '2483100000', ton: '2.4831', deployed: true, usdRate: 5.42, usdtAvailable: true, usdt: '25.5' }),
  GetTonHistory: () => [
    { hash: 'a1', time: sec(now - 40 * min), incoming: false, ton: '0.05', counterparty: 'UQCx1nodeOperatorAddressExample000000000000000', comment: 'echofog: 4.8 GB via node 7f3a', feeTon: '0.004', failed: false },
    { hash: 'a2', time: sec(now - 3 * 60 * min), incoming: false, ton: '0.02', counterparty: 'UQDy2anotherNodeAddressExample0000000000000000', comment: 'echofog: 1.9 GB via node c41e', feeTon: '0.004', failed: false },
    { asset: 'USDT', hash: 'a4', time: sec(now - 5 * 60 * min), incoming: true, ton: '25.5', counterparty: 'UQFriendAddressExample0000000000000000000000000', comment: 'for the proxy', feeTon: '0', failed: false },
    { hash: 'a3', time: sec(now - 26 * 60 * min), incoming: true, ton: '2.5', counterparty: 'EQTelegramWalletHotAddressExample00000000000000', comment: '', feeTon: '0', failed: false },
  ],
  SendTon: () => new Promise((r) => setTimeout(() => r('9f2c4e…mock'), 900)),
  SendUsdt: () => new Promise((r) => setTimeout(() => r('7a1b…mock'), 900)),
  ExportTonRecoveryPhrase: (pw) => { if (pw !== 'mock-test-pass') throw new Error('wrong password'); return 'abandon ability able about above absent absorb abstract absurd abuse access accident account accuse achieve acid acoustic acquire across act action actor actress actual'.split(' '); },
  SetPayAsset: (a) => { payMock.payAsset = a; return null; },
  GetExitPayment: () => ({ ...payMock }),
  SetAutoPay: (on, chunk, cap) => { if (!(Number(chunk) >= 0.01)) throw new Error('enter the top-up amount in USDT, for example 1'); Object.assign(payMock, { autoPay: on, chunkUsdt: String(chunk), dailyCapUsdt: String(cap) }); return null; },
  PayExit: (amt) => new Promise((r) => setTimeout(() => { Object.assign(payMock, { needsPayment: false, pending: true, remainingGb: 20, spentTodayUsdt: String(Number(payMock.spentTodayUsdt) + Number(amt)) }); r('beef...mock'); }, 800)),
  GetVPNStatus: () => ({ ...vpnMock }),
  GetVPNHistory: () => {
    const live = vpnMock.running ? [{ peerId: vpnMock.relayPeerId, addr: '203.0.113.7:443', startedAt: sec(Date.now()) - 600, updatedAt: sec(Date.now()), readyMs: 820, bytesRx: vpnMock.bytesRx || 48e6, bytesTx: vpnMock.bytesTx || 3e6, refused: 788, refusedBy: { 'dial failed': 786, 'timed out': 2 }, remaining: 20.7e9, remainingAt: sec(Date.now()), lastState: vpnMock.exitState, pricePerMb: 49, active: true }] : [];
    return [...live, ...historyMock];
  },
  ClearVPNHistory: () => { historyMock.length = 0; return null; },
  GetSavedExits: () => {
    const all = [...impl.GetVPNHistory()];
    const by = new Map();
    for (const s of all) {
      let e = by.get(s.peerId);
      if (!e) { e = { peerId: s.peerId, addr: s.addr, sessions: 0, bytesRx: 0, bytesTx: 0, refused: 0, lastUsed: s.startedAt, lastReadyMs: s.readyMs, lastState: s.lastState, pricePerMb: s.pricePerMb, priceKnown: true, active: false, ...(exitLabels[s.peerId] || {}) }; by.set(s.peerId, e); }
      Object.assign(e, exitGeo[s.peerId] || {});
      if ((s.remainingAt || 0) > (e.remainingAt || 0)) { e.remaining = s.remaining; e.remainingAt = s.remainingAt; }
      for (const [k, v] of Object.entries(s.refusedBy || {})) { e.refusedBy = e.refusedBy || {}; e.refusedBy[k] = (e.refusedBy[k] || 0) + v; }
      e.sessions++; e.bytesRx += s.bytesRx; e.bytesTx += s.bytesTx; e.refused += s.refused; e.active = e.active || !!s.active;
    }
    return [...by.values()].sort((a, b) => (b.favorite ? 1 : 0) - (a.favorite ? 1 : 0) || b.lastUsed - a.lastUsed);
  },
  SetExitName: (id, name) => { exitLabels[id] = { ...(exitLabels[id] || {}), name: name.trim().slice(0, 40) }; return null; },
  SetExitFavorite: (id, on) => { exitLabels[id] = { ...(exitLabels[id] || {}), favorite: on }; return null; },
  ForgetExit: (id) => { for (let i = historyMock.length - 1; i >= 0; i--) if (historyMock[i].peerId === id) historyMock.splice(i, 1); delete exitLabels[id]; return null; },
  ConnectLastExit: () => new Promise((r, j) => setTimeout(() => {
    if (!historyMock.length) return j(new Error('no exit used yet'));
    Object.assign(vpnMock, { running: true, listenAddr: '127.0.0.1:1080', tunnelReady: true, tunnelNotice: '', exitState: 'ok', relayPeerId: historyMock[0].peerId });
    emit('vpn:started', {});
    r(null);
  }, 700)),
  StartVPN: () => {
    const state = exitMock === 'switch' ? 'ok' : exitMock;
    Object.assign(vpnMock, { running: true, listenAddr: '127.0.0.1:1080', tunnelReady: state === 'ok', tunnelNotice: '', exitState: state, relayPeerId: hex(9) },
      state === 'ok' ? {} : { refused: 7, lastRefusalReason: state === 'quota' ? 'payment' : 'denied', lastRefusalHost: 'youtube.com:443', lastRefusalAt: sec(now) },
      exitMock === 'switch' ? { switches: 1, lastSwitch: { from: hex(8), to: hex(9), reason: 'failing', at: sec(Date.now()) } } : {});
    emit('vpn:started', {});
    return null;
  },
  SetVPNAutoSwitch: (on) => { vpnMock.autoSwitch = on; return null; },
  StopVPN: () => { Object.assign(vpnMock, { running: false, wholeComputer: false, tunnelReady: false }); emit('vpn:stopped', {}); return null; },
  SetWholeComputer: (on) => new Promise((r) => setTimeout(() => { vpnMock.wholeComputer = on; r(null); }, on ? 1500 : 300)),
  ListExitNodes: () => [
    { peerId: hex(9), pricePerMb: 49, claimedKbps: 50000, measuredKbps: 41000, latencyMs: 38, domainsOk: 3, domainsTried: 3, freeTestKb: 512, probes: 4, successes: 4, score: 0.86, connected: true },
    { peerId: hex(10), pricePerMb: 0, claimedKbps: 20000, measuredKbps: 0, latencyMs: 112, domainsOk: 0, domainsTried: 0, freeTestKb: 0, probes: 0, successes: 0, score: 0.4, connected: false },
  ],
  GetSupport: () => ({ ...supportMock }),
  SetSupportPercent: (p) => { if (p < 0 || p > 20) throw new Error('support share must be between 0 and 20 percent'); supportMock.percent = p; return null; },
  GetOnRampURL: () => 'https://example.invalid/buy?address=UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAJKZ',
  PayForTraffic: () => new Promise((r) => setTimeout(() => r('c0ffee…mock'), 900)),
  // the TON deposit for paid nodes: ?vault=none (no deposit yet), ?vault=closing, ?vault=ready (withdrawal can finish)
  GetVault: () => ({ ...vaultMock, canFinish: vaultMock.closing && Date.now() / 1000 >= vaultMock.withdrawAt }),
  VaultDeposit: (a) => new Promise((r, j) => setTimeout(() => {
    const n = Number(a);
    if (!(n > 0)) return j(new Error('enter an amount in TON'));
    if (!vaultMock.deployed && n < Number(vaultMock.minFirstTon)) return j(new Error(`the first deposit must be at least ${vaultMock.minFirstTon} TON`));
    Object.assign(vaultMock, { deployed: true, depositTon: String(+(Number(vaultMock.depositTon) + n - (vaultMock.deployed ? 0 : 0.008)).toFixed(4)) });
    r('d3p0…mock');
  }, 900)),
  VaultRequestWithdraw: () => new Promise((r) => setTimeout(() => { Object.assign(vaultMock, { closing: true, withdrawAt: sec(Date.now()) + 24 * 3600 }); r('w1…mock'); }, 700)),
  VaultCancelWithdraw: () => new Promise((r) => setTimeout(() => { Object.assign(vaultMock, { closing: false, withdrawAt: 0 }); r('c4…mock'); }, 700)),
  VaultExecuteWithdraw: () => new Promise((r, j) => setTimeout(() => {
    if (Date.now() / 1000 < vaultMock.withdrawAt) return j(new Error('the 24 hours are not over yet'));
    Object.assign(vaultMock, { closing: false, withdrawAt: 0, depositTon: '0' });
    r('e5…mock');
  }, 700)),
  GetVoucherEarnings: () => ({ owedTon: '0.7412', cashedTon: '1.2875', quoteNano: 9800000, claimProblem: 'the node wallet has no TON to pay the fee for cashing vouchers', walletAddr: 'UQBmockWalletAddressForGas0000000000000000000000' }),
  GetTonTopUpLink: (a) => 'ton://transfer/UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAJKZ' + (a ? `?amount=${Math.round(Number(a) * 1e9)}` : ''),
};

const groupMembers = [SELF, ALICE, BOB, CAROL, DAVE].map((p) => ({ peerId: p, role: p === SELF ? 'owner' : p === ALICE ? 'admin' : 'member' }));
const groups = [{ id: 'grp_launch', title: 'EchoFog Launch', kind: 'group', members: 5, myRole: 'owner', readOnly: false, description: 'Release room' }];
let probeDomains = ['www.google.com:443', 'www.wikipedia.org:443'];
const settingsMock = { directSocket: false, socketBufferMb: 8 };
const HEX64 = /^[0-9a-f]{64}$/;
const relayMock = {
  settings: { server: true, private: false, signalingOnly: false, allowedPeers: [], relays: ['relay.example.org:4001/' + hex(12)], listenPort: 0 },
  chosen: false,
  startPort: 0,
  connectedAt: {},
};
const relayStatus = () => {
  const s = relayMock.settings;
  const q = new URLSearchParams(location.search);
  const natCode = q.has('cgnat') ? 'cgnat' : q.has('symmetric') ? 'symmetric' : q.has('nat') ? 'nomap' : '';
  const addr = natCode ? '' : `203.0.113.7:${s.listenPort || 51234}/${SELF}`;
  return {
    settings: { ...s, allowedPeers: [...s.allowedPeers], relays: [...s.relays] },
    chosen: relayMock.chosen,
    link: addr ? 'echofog://relay/' + addr : '',
    addr,
    nat: natCode === 'symmetric' ? 'symmetric' : 'cone',
    mapped: !natCode,
    code: natCode,
    reason: natCode ? 'This computer cannot be reached from the Internet.' : '',
    restartNeeded: s.listenPort !== relayMock.startPort,
    activeCircuits: s.server && !s.signalingOnly ? 2 : 0,
    bytesRouted: s.server ? 18350000 : 0,
    forwarded: s.server ? 42 : 0,
    accessRefused: s.private ? 3 : 0,
    using: s.relays.map((r) => {
      const i = r.lastIndexOf('/');
      const id = r.slice(i + 1);
      return { addr: r.slice(0, i), peerId: id, connected: Date.now() > (relayMock.connectedAt[id] || 0) };
    }),
  };
};
// ?hidden=1 starts the mock as a hidden node with somebody already joined
const randomSecret = () => btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(16)))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const accessMock = {
  mode: new URLSearchParams(location.search).has('hidden') ? 'hidden' : 'public',
  nextMode: '',
  secret: randomSecret(),
  secretFromConfig: false,
  network: 'echofog',
  joined: [{ peerId: ALICE, joinedAt: sec(Date.now()) - 86400 * 3, connected: true }, { peerId: hex(31), joinedAt: sec(Date.now()) - 3600, connected: false }],
};
const accessLink = () => {
  const s = accessMock.mode === 'public' ? '' : '&s=' + accessMock.secret;
  const r = accessMock.mode === 'public' ? '' : '&r=' + encodeURIComponent('relay.example.org:4001/' + hex(12));
  return `echofog://join?v=1&net=echofog&p=101&id=${SELF}${s}&a=${encodeURIComponent('203.0.113.7:51234')}${r}`;
};
const sharingMock = { enabled: false, config: { pricePerMb: 49, freeTestKb: 512, uploadKbps: 0, freePeers: [BOB] }, lastPublish: 0, expiresAt: 0, lastError: '', accepted: 12, refused: 1, bytes: 734003200, paymentsReady: true, payAddress: '', earnedUsdt: '0.84' };
const supportMock = { available: true, percent: 5, pendingTon: '0', pendingUsdt: '0.35', address: 'UQ-project-mock' };
const payMock = { active: true, canPay: true, pricePerGb: '0.05', remainingGb: 0.2, needsPayment: true, autoPay: false, chunkUsdt: '1', chunkGb: 20, dailyCapUsdt: '5', spentTodayUsdt: '0', payAsset: 'auto', takesTon: true, chunkTon: '0.311', pending: false, note: '', lastError: '', paysByVault: true };
// ?exit=disabled|quota|unreachable: the exit refuses to serve us after connecting; ?exit=switch: it
// was just replaced by another one
const exitMock = new URLSearchParams(location.search).get('exit') || 'ok';
const vaultStage = new URLSearchParams(location.search).get('vault');
const vaultMock = {
  address: 'EQDvau1tMockAddressExample00000000000000000000000', deployed: vaultStage !== 'none', depositTon: vaultStage === 'none' ? '0' : '3.25',
  closing: vaultStage === 'closing' || vaultStage === 'ready', withdrawAt: vaultStage === 'ready' ? sec(now) - 60 : vaultStage === 'closing' ? sec(now) + 17 * 3600 + 23 * 60 : 0,
  canFinish: false, minFirstTon: '0.1',
};
const exitGeo = { '5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d': { countryCode: 'NL', city: 'Groningen' }, 'beab718659cea03fbe747c2f1e5ffbef4fd32e9a14bf550802d89b19138a1bf6': { countryCode: 'DE', city: 'Frankfurt am Main' } };
const exitLabels = { '5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d': { name: 'Мой GCP', favorite: true } };
// exits used before, newest first, as the core's history returns them
const historyMock = [
  { peerId: '5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d', addr: '34.158.189.116:443', startedAt: sec(Date.now()) - 3 * 3600, endedAt: sec(Date.now()) - 2 * 3600, updatedAt: sec(Date.now()) - 2 * 3600, readyMs: 640, bytesRx: 1.9e9, bytesTx: 85e6, refused: 309, refusedBy: { 'dial failed': 305, 'timed out': 4 }, remaining: 20.73e9, remainingAt: sec(Date.now()) - 2 * 3600, lastState: 'ok', pricePerMb: 49, endReason: 'stopped' },
  { peerId: '5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d', addr: '34.158.189.116:443', startedAt: sec(Date.now()) - 5 * 3600, endedAt: sec(Date.now()) - 4.8 * 3600, updatedAt: sec(Date.now()) - 4.8 * 3600, readyMs: 820, bytesRx: 391e3, bytesTx: 307e3, refused: 6, lastState: 'ok', pricePerMb: 49, endReason: 'stopped' },
  { peerId: 'beab718659cea03fbe747c2f1e5ffbef4fd32e9a14bf550802d89b19138a1bf6', addr: '34.32.224.253:443', startedAt: sec(Date.now()) - 26 * 3600, endedAt: sec(Date.now()) - 25 * 3600, updatedAt: sec(Date.now()) - 25 * 3600, readyMs: 1450, bytesRx: 420e6, bytesTx: 12e6, refused: 4, lastState: 'ok', pricePerMb: 0, endReason: 'switched' },
  { peerId: '3df2ff7f53a1d4772d030a628c21028b20abebc573a5fcb692224ae7366b5140', addr: '', startedAt: sec(Date.now()) - 50 * 3600, endedAt: sec(Date.now()) - 49.5 * 3600, updatedAt: sec(Date.now()) - 49.5 * 3600, readyMs: 0, bytesRx: 0, bytesTx: 0, refused: 12, lastState: 'quota', pricePerMb: 98, endReason: 'app_closed' },
];
const vpnMock = { running: false, listenAddr: '', relayPeerId: '', activeConns: 3, bytesRx: 48211000, bytesTx: 5120000, tunnelReady: false, tunnelNotice: '', exitState: '', refused: 0, lastRefusalReason: '', lastRefusalHost: '', lastRefusalAt: 0, autoSwitch: true, searching: false, switches: 0, lastSwitch: { from: '', to: '', reason: '', at: 0 }, wholeComputer: false, wholeComputerSupported: true, wholeComputerError: '' };

// handy in the console: __emit('group:event', {kind: 'call', groupId: 'grp_launch', title: 'EchoFog Launch', from: '...', callId: 'x', coordinator: '...'})
window.__emit = (name, data) => runtimeImpl.EventsEmit(name, data);
window.__ids = { SELF, ALICE, BOB, CAROL, DAVE };

window.go = {
  main: {
    App: new Proxy({}, {
      get(_, name) {
        if (impl[name]) return async (...a) => impl[name](...a);
        if (String(name).startsWith('List')) return async () => [];
        if (String(name).startsWith('Get')) return async () => ({});
        return async () => undefined;
      },
    }),
  },
};

// No permission prompts in the preview: synthetic mic, camera and screen.
const synthetic = (kind) => {
  if (kind === 'audio') {
    const ctx = new AudioContext();
    const dest = ctx.createMediaStreamDestination();
    return dest.stream;
  }
  const c = document.createElement('canvas');
  c.width = 1280;
  c.height = 720;
  painter(c).set({ owner: SELF, kind: 'screen', title: 'my-screen.png' });
  return c.captureStream(30);
};
navigator.mediaDevices.getUserMedia = async (c) => (c.video ? synthetic('video') : synthetic('audio'));
// a shared screen comes with sound unless the URL has &nosound (as when "share audio" is not ticked)
navigator.mediaDevices.getDisplayMedia = async () => {
  const s = synthetic('video');
  if (!new URLSearchParams(location.search).has('nosound')) synthetic('audio').getAudioTracks().forEach((t) => s.addTrack(t));
  return s;
};
callService.muteOutput = true; // keep the preview silent

if (new URLSearchParams(location.search).has('incoming')) {
  // ring once the app is logged in and listening
  const wait = setInterval(() => {
    if (!listeners.get('call:incoming')?.size) return;
    clearInterval(wait);
    setTimeout(() => emit('call:incoming', { callId: 'call_in', coordinator: ALICE, from: ALICE, timestamp: Date.now() }), 1500);
  }, 200);
}

console.info('[wails-mock] fake Go core active');
