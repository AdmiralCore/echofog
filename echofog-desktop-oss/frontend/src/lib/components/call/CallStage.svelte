<script>
    import { fade } from 'svelte/transition';
    import { callService, callState, videoStreams } from '../../services/call.service.js';
    import { callDuration, formatDuration, stageExpanded, waitingKey } from '../../stores/call.store.js';
    import ParticipantTile from './ParticipantTile.svelte';
    import StreamTile from './StreamTile.svelte';
    import ParticipantMenu from './ParticipantMenu.svelte';
    import CallControls from './CallControls.svelte';
    import Icon from '../ui/Icon.svelte';
    import { t } from '../../i18n/index.js';

    export let title = '';
    /** maximized: the stage takes the whole chat column */
    export let maximized = false;

    let menu = null; // { peer, x, y }

    $: s = $callState;
    $: members = orderMembers(s.members.length ? s.members : s.self ? [s.self] : [], s.self);
    // Camera streams render inside their owner's tile; screens get their own tiles.
    $: cameraOf = cameraMap(s, $videoStreams);
    $: screens = s.activeStreams.filter((st) => st.mediaType !== 'camera');
    $: focused = s.focusedStreamId ? s.activeStreams.find((st) => st.streamId === s.focusedStreamId) : null;
    $: tiles = screens.length + members.length;
    // every tile is the same 16:9 box; the column count is whichever makes the boxes biggest
    const GAP = 12;
    let gridW = 0;
    let gridH = 0;
    $: tile = fitTiles(tiles, gridW, gridH);

    function fitTiles(n, W, H) {
        if (!n || W <= 0 || H <= 0) return { w: 0, h: 0 };
        let best = { w: 0, h: 0 };
        for (let cols = 1; cols <= n; cols++) {
            const rows = Math.ceil(n / cols);
            let w = (W - GAP * (cols - 1)) / cols;
            let h = (w * 9) / 16;
            if (h * rows + GAP * (rows - 1) > H) {
                h = (H - GAP * (rows - 1)) / rows;
                w = (h * 16) / 9;
            }
            if (w > best.w) best = { w: Math.floor(w), h: Math.floor(h) };
        }
        return best;
    }

    function orderMembers(list, self) {
        // you first, then everybody else in a stable order
        return [...list].sort((a, b) => (a === self ? -1 : b === self ? 1 : a.localeCompare(b)));
    }

    function cameraMap(state, videos) {
        const out = {};
        for (const st of state.activeStreams) {
            if (st.mediaType !== 'camera') continue;
            const media = callService.streamFor(st.streamId, state, videos);
            if (media) out[st.broadcaster] = media;
        }
        return out;
    }

    function openMenu(e) {
        menu = e.detail;
    }
</script>

<section class="relative flex flex-col w-full h-full min-h-0 bg-[#000] text-white select-none" aria-label={$t('call.call')}>
    <header class="h-12 shrink-0 flex items-center justify-between px-4 gap-3">
        <div class="flex items-center gap-2 min-w-0">
            <span class="w-2 h-2 rounded-full {s.inCall ? 'bg-[#23a55a]' : 'bg-[#f0b232] animate-pulse'}"></span>
            <span class="font-semibold truncate">{title || $t('call.call')}</span>
            <span class="text-sm text-[#949ba4] shrink-0">
                {#if s.inCall}{formatDuration($callDuration)} · {$t('call.people', { n: members.length })}{:else}{$t(waitingKey(s))}{/if}
            </span>
            <span class="hidden sm:flex items-center gap-1 text-xs text-[#949ba4] ml-2" title={$t('call.e2eTitle')}>
                <Icon name="lock" size={13} /> {$t('call.e2e')}
            </span>
        </div>
        <div class="flex items-center gap-1">
            <button class="hdr-btn" title={maximized ? $t('call.showChat') : $t('call.focusCall')} aria-label={maximized ? $t('call.showChat') : $t('call.focusCall')} on:click={() => (maximized = !maximized)}>
                <Icon name={maximized ? 'collapse' : 'expand'} size={18} />
            </button>
            <button class="hdr-btn" title={$t('call.minimize')} aria-label={$t('call.minimize')} on:click={() => stageExpanded.set(false)}>
                <Icon name="chevronUp" size={18} />
            </button>
        </div>
    </header>

    <div class="flex-1 min-h-0 px-3 pb-3">
        {#if focused}
            <div class="flex gap-3 w-full h-full min-h-0">
                <div class="flex-1 min-w-0 min-h-0">
                    <StreamTile stream={focused} focused />
                </div>
                <div class="w-56 shrink-0 flex flex-col gap-3 overflow-y-auto pr-1">
                    {#each screens.filter((st) => st.streamId !== focused.streamId) as st (st.streamId)}
                        <div class="aspect-video shrink-0"><StreamTile stream={st} compact /></div>
                    {/each}
                    {#each members as peer (peer)}
                        <div class="aspect-video shrink-0"><ParticipantTile {peer} camera={cameraOf[peer]} compact on:menu={openMenu} /></div>
                    {/each}
                </div>
            </div>
        {:else}
            <div class="relative w-full h-full min-h-0 overflow-hidden" bind:clientWidth={gridW} bind:clientHeight={gridH}>
                <div class="absolute inset-0 flex flex-wrap content-center justify-center" style="gap:{GAP}px">
                    {#each screens as st (st.streamId)}
                        <div class="shrink-0" style="width:{tile.w}px;height:{tile.h}px"><StreamTile stream={st} /></div>
                    {/each}
                    {#each members as peer (peer)}
                        <div class="shrink-0" style="width:{tile.w}px;height:{tile.h}px"><ParticipantTile {peer} camera={cameraOf[peer]} on:menu={openMenu} /></div>
                    {/each}
                </div>
            </div>
        {/if}
    </div>

    <footer class="shrink-0 flex justify-center pb-4">
        <CallControls />
    </footer>

    {#if s.notice || s.streamError}
        <div class="absolute left-1/2 -translate-x-1/2 bottom-24 max-w-[90%] rounded-lg bg-[#111214] border border-white/10 px-4 py-2.5 text-sm shadow-xl {s.streamError ? 'text-[#f23f43]' : 'text-[#dbdee1]'}" role="status" transition:fade={{ duration: 150 }}>
            {s.streamError || s.notice}
        </div>
    {/if}

    {#if menu}
        <ParticipantMenu peer={menu.peer} x={menu.x} y={menu.y} on:close={() => (menu = null)} />
    {/if}
</section>

<style>
    .hdr-btn {
        width: 34px;
        height: 34px;
        border-radius: 8px;
        display: flex;
        align-items: center;
        justify-content: center;
        color: #b5bac1;
        padding: 0;
        border: none;
        background: transparent;
    }
    .hdr-btn:hover:not(:disabled) {
        background: rgba(255, 255, 255, 0.08);
        border: none;
        color: #fff;
    }
</style>
