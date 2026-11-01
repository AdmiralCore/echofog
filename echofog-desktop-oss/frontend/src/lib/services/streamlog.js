// Stream logs: while one is on, a stream's stats are recorded once a second — the same numbers its
// tile shows — together with the call's log lines of that time. When it stops (by hand, or because
// the stream or the call ended) the log is put together for the dialog that saves it or sends it
// into a chat, so a stutter can be looked at afterwards instead of described from memory.
//
// It names nobody: peer IDs in the log lines become p1, p2…, the call's ID is left out, and the
// app's log already masks IP addresses.

import { writable, get } from 'svelte/store';
import { App } from '../api.js';
import { callService, callState, videoStreams } from './call.service.js';
import { activeGroupCalls, activeId } from '../stores/chats.js';
import { rates, stillSecond } from './streamrates.js';

const MAX_SECONDS = 3 * 3600;
const MAX_LOG_LINES = 4000;

/** What each recorded second holds, in this order. */
export const COLUMNS = [
    't', 'fps', 'captureFps', 'encodedFps', 'receivedFps', 'droppedFps', 'width', 'height', 'mbps', 'codecMs', 'bufferMs',
    'packets', 'lostPkts', 'gaps', 'nacked', 'fec', 'retransmitted', 'netIn', 'netOut', 'qDropped', 'qFailed',
    'sendKbps', 'freezes', 'freezeSec', 'pli', 'nack', 'keyframes', 'limit',
];

/** Streams being recorded: streamId -> { startedAt, seconds } */
export const recordings = writable({});
/** The log that just finished, for the dialog: { streamId, own, why, summary, text, name } */
export const finishedLog = writable(null);

const active = new Map(); // streamId -> recorder

/** Starts recording a stream of the current call; own is true for a stream this app sends. */
export function startRecording(streamId, own) {
    if (active.has(streamId)) return;
    const s = get(callState);
    const info = s.activeStreams.find((x) => x.streamId === streamId) || {};
    const groupCall = Object.entries(get(activeGroupCalls)).find(([, c]) => c.callId === s.callId);
    const rec = {
        own, started: Date.now(), prev: null, samples: [], busy: false, impl: '', hw: null, truncated: false,
        chatId: groupCall ? groupCall[0] : get(activeId), // the call's group chat, or the chat on screen
        stream: {
            own, mediaType: info.mediaType || '', quality: info.qualityId || '', width: info.width || 0, height: info.height || 0,
            fps: info.fps || 0, bitrate: info.bitrate || 0, mode: own && info.mediaType === 'screen' ? s.screenMode : undefined,
        },
        members: [s.members.length],
    };
    rec.timer = setInterval(() => tick(streamId, rec), 1000);
    active.set(streamId, rec);
    recordings.update((m) => ({ ...m, [streamId]: { startedAt: rec.started, seconds: 0 } }));
    tick(streamId, rec);
}

/** Stops recording a stream and puts its log together for the dialog. */
export function stopRecording(streamId, why = 'stopped') {
    const rec = active.get(streamId);
    if (!rec) return;
    clearInterval(rec.timer);
    active.delete(streamId);
    recordings.update((m) => {
        const { [streamId]: _, ...rest } = m;
        return rest;
    });
    buildLog(streamId, rec, why).then((log) => finishedLog.set(log));
}

async function tick(streamId, rec) {
    const s = get(callState);
    if (!s.inCall || !s.activeStreams.some((x) => x.streamId === streamId)) {
        stopRecording(streamId, s.inCall ? 'stream ended' : 'call ended');
        return;
    }
    if (rec.busy) return;
    rec.busy = true;
    try {
        const media = rec.own ? null : callService.streamFor(streamId, s, get(videoStreams));
        const snap = await callService.tileStats(streamId, media, rec.own);
        if (rec.prev && snap.at > rec.prev.at) {
            const r = rates(rec.prev, snap, rec.own);
            r.t = Math.round((Date.now() - rec.started) / 1000);
            r.mbps = Math.round(r.mbps * 100) / 100;
            r.codecMs = Math.round(r.codecMs * 10) / 10;
            r.bufferMs = Math.round(r.bufferMs);
            r.freezeSec = Math.round(r.freezeSec * 10) / 10;
            if (r.impl) rec.impl = r.impl;
            if (r.hw != null) rec.hw = r.hw;
            rec.samples.push(COLUMNS.map((c) => r[c] ?? (c === 'limit' ? '' : 0)));
            if (rec.samples.length > MAX_SECONDS) {
                rec.samples.shift();
                rec.truncated = true;
            }
            if (!rec.members.includes(s.members.length)) rec.members.push(s.members.length);
        }
        rec.prev = snap;
        recordings.update((m) => (m[streamId] ? { ...m, [streamId]: { startedAt: rec.started, seconds: rec.samples.length } } : m));
    } finally {
        rec.busy = false;
    }
}

// --- the log --------------------------------------------------------------------------------

const col = (name) => COLUMNS.indexOf(name);

function percentile(sorted, p) {
    if (!sorted.length) return 0;
    return sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))];
}

/** summarize(samples, own): the totals and spreads a reader looks at first. */
export function summarize(samples, own) {
    const pick = (name) => samples.map((r) => r[col(name)]);
    const sum = (name) => pick(name).reduce((a, b) => a + (+b || 0), 0);
    const fps = pick('fps').slice().sort((a, b) => a - b);
    const mbps = pick('mbps').slice().sort((a, b) => a - b);
    const last = samples[samples.length - 1] || [];
    const first = samples[0] || [];
    const delta = (name) => Math.max(0, (last[col(name)] || 0) - (first[col(name)] || 0));
    const limits = {};
    for (const l of pick('limit')) if (l) limits[l] = (limits[l] || 0) + 1;
    const sizes = {};
    for (const r of samples) {
        const k = `${r[col('width')]}x${r[col('height')]}`;
        sizes[k] = (sizes[k] || 0) + 1;
    }
    const size = Object.entries(sizes).sort((a, b) => b[1] - a[1])[0]?.[0] || '';
    const out = {
        seconds: samples.length,
        fps: { avg: samples.length ? Math.round((fps.reduce((a, b) => a + b, 0) / fps.length) * 10) / 10 : 0, median: percentile(fps, 50), p5: percentile(fps, 5), min: fps[0] || 0, max: fps[fps.length - 1] || 0 },
        mbps: { median: percentile(mbps, 50), max: mbps[mbps.length - 1] || 0 },
        size,
        secondsWithTrouble: samples.filter((r) => (own ? r[col('qDropped')] + r[col('qFailed')] + r[col('netOut')] : r[col('lostPkts')] + r[col('gaps')] + r[col('netIn')]) > 0).length,
        limits,
    };
    // freezes the network caused: a still picture's waits for the next frame are counted apart
    let freezes = 0, freezeSec = 0, still = 0;
    const at = (r, name) => +r[col(name)] || 0;
    for (let i = 1; i < samples.length; i++) {
        const r = samples[i], p = samples[i - 1];
        if (stillSecond({ lostPkts: at(r, 'lostPkts'), gaps: at(r, 'gaps'), netIn: at(r, 'netIn'), packets: at(r, 'packets') })) {
            still++;
            continue;
        }
        freezes += Math.max(0, at(r, 'freezes') - at(p, 'freezes'));
        freezeSec += Math.max(0, at(r, 'freezeSec') - at(p, 'freezeSec'));
    }
    if (own) {
        Object.assign(out, { dropped: sum('qDropped'), failed: sum('qFailed'), netOut: sum('netOut'), keyframesServed: delta('keyframes'), retransmitted: sum('retransmitted') });
    } else {
        Object.assign(out, {
            lostPackets: sum('lostPkts'), gaps: sum('gaps'), nacked: sum('nacked'), fecRecovered: sum('fec'), netIn: sum('netIn'),
            framesDropped: sum('droppedFps'), freezes, freezeSeconds: Math.round(freezeSec * 10) / 10, stillSeconds: still, keyframesAsked: delta('pli'),
        });
    }
    return out;
}

// peer IDs in log lines (whole or shortened hex) become p1, p2… in the order they appear
function anonymizer() {
    const names = new Map();
    const alias = (id) => {
        if (!names.has(id)) names.set(id, 'p' + (names.size + 1));
        return names.get(id);
    };
    return (s) => String(s).replace(/\b[0-9a-f]{12,64}\b/gi, alias);
}

function logLine(e, anon) {
    const attrs = Object.entries(e.attrs || {})
        .filter(([k]) => k !== 'call_id' && k !== 'callId')
        .map(([k, v]) => `${k}=${anon(v)}`)
        .join(' ');
    const at = new Date(e.time);
    const time = isNaN(at) ? '' : at.toISOString().slice(11, 23);
    return `${time} ${String(e.level || '').toUpperCase()} ${anon(e.message)}${attrs ? ' ' + attrs : ''}`;
}

async function buildLog(streamId, rec, why) {
    const anon = anonymizer();
    let lines = [];
    try {
        const entries = (await App.GetLogs(0)) || [];
        lines = entries
            .filter((e) => Date.parse(e.time) >= rec.started - 5000 && /^(ui-)?call/.test(e.attrs?.component || ''))
            .slice(-MAX_LOG_LINES)
            .map((e) => logLine(e, anon));
    } catch (_) { /* no logs: the numbers alone still help */ }
    let version = '';
    try { version = await App.AppVersion(); } catch (_) { /* an older app */ }

    const summary = summarize(rec.samples, rec.own);
    const started = new Date(rec.started);
    const head = {
        format: 'echofog-stream-log/1',
        app: version,
        platform: navigator.platform || '',
        started: started.toISOString(),
        ended: why,
        stream: rec.stream,
        members: rec.members,
        coder: { impl: rec.impl, hardware: rec.hw },
        truncated: rec.truncated,
        summary,
    };
    // one second per line, so the file reads in a text editor too
    let text = '{\n';
    for (const [k, v] of Object.entries(head)) text += `${JSON.stringify(k)}: ${JSON.stringify(v)},\n`;
    text += `"columns": ${JSON.stringify(COLUMNS)},\n"samples": [\n${rec.samples.map((r) => JSON.stringify(r)).join(',\n')}\n],\n`;
    text += `"log": [\n${lines.map((l) => JSON.stringify(l)).join(',\n')}\n]\n}\n`;

    const pad = (n) => String(n).padStart(2, '0');
    const name = `echofog-stream-${started.getFullYear()}-${pad(started.getMonth() + 1)}-${pad(started.getDate())}-${pad(started.getHours())}${pad(started.getMinutes())}${pad(started.getSeconds())}.json`;
    return { streamId, own: rec.own, why, chatId: rec.chatId, summary, head, text, name, lines: lines.length };
}

/** summaryText(log): a few lines to paste where a file does not fit. */
export function summaryText(log) {
    const s = log.summary, h = log.head;
    const st = h.stream;
    const out = [
        `EchoFog stream log — ${h.started} — ${s.seconds} s — ${log.own ? 'sending' : 'watching'} ${st.mediaType} ${st.quality}`.trim(),
        `app ${h.app || '?'} · ${h.coder.impl || '?'}${h.coder.hardware === true ? ' (GPU)' : h.coder.hardware === false ? ' (CPU)' : ''} · ${s.size} · members ${h.members.join('/')}`,
        `fps avg ${s.fps.avg} · median ${s.fps.median} · 5% ${s.fps.p5} · min ${s.fps.min} · max ${s.fps.max} · Mbit/s median ${s.mbps.median}`,
    ];
    if (log.own) out.push(`queue dropped ${s.dropped} · failed ${s.failed} · transport dropped ${s.netOut} · keyframes served ${s.keyframesServed} · resent ${s.retransmitted}`);
    else out.push(`lost ${s.lostPackets} · gaps ${s.gaps} · resent ${s.nacked} · FEC ${s.fecRecovered} · no room ${s.netIn} · freezes ${s.freezes} (${s.freezeSeconds} s) · still picture ${s.stillSeconds} s · keyframes asked ${s.keyframesAsked}`);
    out.push(`seconds with trouble ${s.secondsWithTrouble}${Object.keys(s.limits).length ? ' · limited by ' + Object.entries(s.limits).map(([k, v]) => `${k} ${v} s`).join(', ') : ''}`);
    return out.join('\n');
}
