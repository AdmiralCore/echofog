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
    [false, 'Hey! Did you see the new relay trees? 4K60 between three of us without a server 🤯', now - 26 * 60 * min],
    [true, 'Yes, the 10-node test passed this morning', now - 26 * 60 * min + 2 * min],
    [true, 'Heal time after a relay crash is about a second now', now - 26 * 60 * min + 3 * min],
    [false, 'Nice. Want to jump on a call and I share my screen?', now - 12 * min],
    [false, 'I have the launch checklist open', now - 11 * min],
    [true, 'Sure, calling you in a sec', now - 2 * min],
  ],
  [BOB]: [[false, 'Pushed the TON wallet draft, take a look when you can', now - 3 * 60 * min]],
  [CAROL]: [[true, 'See you tomorrow', now - 2 * 24 * 60 * min]],
};
const history = Object.fromEntries(Object.entries(dm).map(([peer, list]) => [peer, list.map(([mine, text, ts], i) => ({
  id: `${peer.slice(0, 4)}-${i}`, text, timestamp: sec(ts), is_mine: mine, sender_id: mine ? SELF : peer, target_id: mine ? peer : SELF,
}))]));
const groupMsgs = [
  [ALICE, 'Release candidate is building, ETA 20 minutes', now - 50 * min],
  [BOB, 'Node runner guide is updated with the Docker one-liner', now - 41 * min],
  [BOB, 'Also added the price-per-GB section', now - 40 * min],
  [SELF, 'Great. Let us do a quick voice sync at 18:00', now - 30 * min],
  [CAROL, '👍', now - 29 * min],
].map(([from, text, ts], i) => ({ id: 'g' + i, sender: from, text, timestamp: ts, isMine: from === SELF, delivered: 4, failed: 0 }));

const qualities = [
  ['360p30', '360p 30 fps', 640, 360, 30, 800], ['720p30', '720p 30 fps', 1280, 720, 30, 2500], ['720p60', '720p 60 fps', 1280, 720, 60, 4000],
  ['1080p30', '1080p 30 fps', 1920, 1080, 30, 5000], ['1080p60', '1080p 60 fps', 1920, 1080, 60, 8000], ['1440p60', '1440p 60 fps', 2560, 1440, 60, 14000],
  ['2160p30', '4K 30 fps', 3840, 2160, 30, 16000], ['2160p60', '4K 60 fps', 3840, 2160, 60, 25000],
].map(([id, label, width, height, fps, bitrateKbps]) => ({ id, label, width, height, fps, bitrateKbps, codec: 'h264', fits: bitrateKbps <= 30000 }));

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
  pc.addTransceiver('audio', { direction: 'recvonly' });
  pc.addTransceiver('video', { direction: 'recvonly' });
  pc.addTransceiver('video', { direction: 'recvonly' });
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
}

function unwatch(streamId) {
  const slot = Object.keys(call.slots).find((k) => call.slots[k].streamId === streamId && k.startsWith('v'));
  if (!slot) return;
  delete call.slots[slot];
  emit('call:media_slot', { slot, kind: 'video', peer: '', streamId, active: false });
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
  SelectProfile: () => null,
  IsRegistered: () => true,
  Login: () => null,
  StartNode: () => null,
  GetMyID: () => SELF,
  GetContacts: () => contacts,
  GetChatHistory: (peer) => [...(history[peer] || [])].reverse(),
  SendMessage: (pub, text) => {
    const c = contacts.find((x) => x.pub_key === pub);
    const msg = { id: 'm' + Date.now(), text, timestamp: sec(Date.now()), is_mine: true, sender_id: SELF, target_id: c.peer_id };
    history[c.peer_id] = [...(history[c.peer_id] || []), msg];
    return null;
  },
  GetPeerBalance: () => 0,
  ListGroups: () => [{ id: 'grp_launch', title: 'EchoFog Launch', kind: 'group', members: 5, myRole: 'owner', readOnly: false, description: 'Release room' }],
  GetGroupMessages: () => groupMsgs,
  SendGroupMessage: (id, text) => {
    const m = { id: 'g' + Date.now(), sender: SELF, text, timestamp: Date.now(), isMine: true, delivered: 4, failed: 0 };
    groupMsgs.push(m);
    return m;
  },
  ListGroupMembers: () => [SELF, ALICE, BOB, CAROL, DAVE].map((p) => ({ peerId: p, role: p === SELF ? 'owner' : 'member', isMe: p === SELF })),
  GetGroupInvite: () => 'fogroup1:demo-invite',
  InviteToCall: () => ({ callId: 'call_demo', coordinator: SELF }),
  GetStreamQualities: () => qualities,
  MeasureUploadCapacity: () => new Promise((r) => setTimeout(() => r(30000), 700)),
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
  GetExitPayment: () => ({ ...payMock }),
  SetAutoPay: (on, chunk, cap) => { if (!(Number(chunk) >= 0.01)) throw new Error('enter the top-up amount in USDT, for example 1'); Object.assign(payMock, { autoPay: on, chunkUsdt: String(chunk), dailyCapUsdt: String(cap) }); return null; },
  PayExit: (amt) => new Promise((r) => setTimeout(() => { Object.assign(payMock, { needsPayment: false, pending: true, remainingGb: 20, spentTodayUsdt: String(Number(payMock.spentTodayUsdt) + Number(amt)) }); r('beef...mock'); }, 800)),
  GetVPNStatus: () => ({ ...vpnMock }),
  GetSharingStatus: () => ({ enabled: false, config: { pricePerMb: 1, freeTestKb: 512, uploadKbps: 0 }, lastPublish: 0, expiresAt: 0, lastError: '', accepted: 0, refused: 0, bytes: 0 }),
  StartVPN: () => { Object.assign(vpnMock, { running: true, listenAddr: '127.0.0.1:1080', tunnelReady: true, tunnelNotice: '', relayPeerId: hex(9) }); emit('vpn:started', {}); return null; },
  StopVPN: () => { Object.assign(vpnMock, { running: false, wholeComputer: false, tunnelReady: false }); emit('vpn:stopped', {}); return null; },
  SetWholeComputer: (on) => new Promise((r) => setTimeout(() => { vpnMock.wholeComputer = on; r(null); }, on ? 1500 : 300)),
  ListExitNodes: () => [{ peerId: hex(9), pricePerMb: 49, claimedKbps: 50000, measuredKbps: 41000, latencyMs: 38, domainsOk: 3, domainsTried: 3, freeTestKb: 512, probes: 4, successes: 4, score: 0.86, connected: true }],
  GetSupport: () => ({ ...supportMock }),
  SetSupportPercent: (p) => { if (p < 0 || p > 20) throw new Error('support share must be between 0 and 20 percent'); supportMock.percent = p; return null; },
  GetOnRampURL: () => 'https://example.invalid/buy?address=UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAJKZ',
  PayForTraffic: () => new Promise((r) => setTimeout(() => r('c0ffee…mock'), 900)),
  GetTonTopUpLink: (a) => 'ton://transfer/UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAJKZ' + (a ? `?amount=${Math.round(Number(a) * 1e9)}` : ''),
};

const supportMock = { available: true, percent: 5, pendingTon: '0', pendingUsdt: '0.35', address: 'UQ-project-mock' };
const payMock = { active: true, canPay: true, pricePerGb: '0.05', remainingGb: 0.2, needsPayment: true, autoPay: false, chunkUsdt: '1', chunkGb: 20, dailyCapUsdt: '5', spentTodayUsdt: '0', pending: false, note: '', lastError: '' };
const vpnMock = { running: false, listenAddr: '', relayPeerId: '', activeConns: 3, bytesRx: 48211000, bytesTx: 5120000, tunnelReady: false, tunnelNotice: '', wholeComputer: false, wholeComputerSupported: true, wholeComputerError: '' };

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
navigator.mediaDevices.getDisplayMedia = async () => synthetic('video');
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
