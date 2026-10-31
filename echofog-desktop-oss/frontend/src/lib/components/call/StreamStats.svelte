<script>
    /** Live stats over a stream's tile, once a second: the frame rate over the last minute as a
     *  graph (red marks: a second with losses or a freeze), and where frames go along the way —
     *  for our own stream capture → encoder → network, for a watched one network → received →
     *  shown. Enough to tell a slow computer from a lossy path. */
    import { onDestroy } from 'svelte';
    import { callService } from '../../services/call.service.js';
    import { rates } from '../../services/streamrates.js';
    import { t } from '../../i18n/index.js';

    export let streamId;
    export let media = null;
    export let own = false;

    const POINTS = 60;
    let prev = null;
    let now = null; // the rates of the last second
    let hist = []; // [{ fps, bad }]
    let peakFps = 30;

    async function tick() {
        const snap = await callService.tileStats(streamId, media, own);
        if (prev && snap.at > prev.at) now = rates(prev, snap, own);
        prev = snap;
        if (now) {
            hist = [...hist.slice(-(POINTS - 1)), { fps: now.fps, bad: now.bad }];
            peakFps = Math.max(30, ...hist.map((h) => h.fps));
        }
    }
    tick();
    const timer = setInterval(tick, 1000);
    onDestroy(() => clearInterval(timer));

    const W = 220, H = 44;
    $: line = hist.map((h, i) => `${((i + POINTS - hist.length) / (POINTS - 1)) * W},${H - (h.fps / peakFps) * (H - 4) - 2}`).join(' ');
    const coder = (r) => (r.hw === true ? $t('call.st.gpu') : r.hw === false ? $t('call.st.cpu') : r.impl || '—');
    const n1 = (v) => (v >= 10 ? Math.round(v) : Math.round(v * 10) / 10);
</script>

<div class="stats" role="status" aria-label={$t('call.st.title')}>
    <div class="head">
        <span>{$t('call.st.title')}</span>
        {#if now}<b>{own ? now.encodedFps : now.shownFps} {$t('call.fps')}</b>{/if}
    </div>
    <svg viewBox="0 0 {W} {H}" width={W} height={H} aria-hidden="true">
        <line x1="0" x2={W} y1={H - (60 / peakFps) * (H - 4) - 2} y2={H - (60 / peakFps) * (H - 4) - 2} class="ref" />
        {#each hist as h, i}
            {#if h.bad}<rect x={((i + POINTS - hist.length) / (POINTS - 1)) * W - 1.5} y="0" width="3" height={H} class="bad" />{/if}
        {/each}
        {#if hist.length > 1}<polyline points={line} class="fps" />{/if}
    </svg>
    {#if now}
        {#if own}
            <p>{$t('call.st.ownFrames', { cap: now.captureFps, enc: now.encodedFps, sent: now.sentFps })}</p>
            <p>{now.width}×{now.height} · {n1(now.mbps)} {$t('unit.mbps')}{#if now.sendKbps}{' '}({$t('call.st.cap', { v: n1(now.sendKbps / 1000) })}){/if} · {coder(now)} · {n1(now.codecMs)} {$t('call.st.ms')}</p>
            {#if now.limit}<p class="warn">{$t('call.enc.limit.' + now.limit)}</p>{/if}
            <p class:warn={now.qDropped + now.qFailed + now.netOut > 0}>{$t('call.st.ownNet', { units: now.units, dropped: now.qDropped + now.netOut, failed: now.qFailed })}</p>
            <p>{$t('call.st.keyframes', { n: now.keyframes })}</p>
        {:else}
            <p>{$t('call.st.viewFrames', { recv: now.receivedFps, shown: now.shownFps, dropped: now.droppedFps })}</p>
            <p>{now.width}×{now.height} · {n1(now.mbps)} {$t('unit.mbps')} · {coder(now)} · {n1(now.codecMs)} {$t('call.st.ms')}</p>
            <p class:warn={now.lostPkts + now.gaps + now.netIn > 0}>{$t('call.st.viewNet', { units: now.units, lost: now.lostPkts, gaps: now.gaps, nacked: now.nacked, fec: now.fec, dropped: now.netIn })}</p>
            <p class:warn={now.freezes > 0}>{$t('call.st.freezes', { n: now.freezes, s: n1(now.freezeSec), pli: now.pli, buf: Math.round(now.bufferMs) })}</p>
        {/if}
    {:else}
        <p>{$t('call.st.reading')}</p>
    {/if}
</div>

<style>
    .stats {
        position: absolute; right: 8px; bottom: 8px; z-index: 5; width: 240px; padding: 8px 10px 9px;
        border-radius: 10px; background: rgba(8, 10, 16, 0.78); backdrop-filter: blur(6px);
        color: #e8eaf0; font-size: 11px; line-height: 1.45; font-variant-numeric: tabular-nums; pointer-events: none;
    }
    .head { display: flex; justify-content: space-between; align-items: baseline; margin-bottom: 4px; font-weight: 650; }
    .head b { font-size: 13px; color: #7ee2a8; }
    svg { display: block; margin-bottom: 4px; }
    .fps { fill: none; stroke: #7ee2a8; stroke-width: 1.6; stroke-linejoin: round; }
    .ref { stroke: rgba(255, 255, 255, 0.18); stroke-dasharray: 3 3; }
    .bad { fill: rgba(242, 63, 67, 0.55); }
    p { margin: 0; color: rgba(232, 234, 240, 0.85); }
    p.warn { color: #ffb86b; }
</style>
