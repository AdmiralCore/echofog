<script>
    import { fade } from 'svelte/transition';
    import { callService, callState, videoStreams } from '../../services/call.service.js';
    import { callDuration, formatDuration, stageExpanded } from '../../stores/call.store.js';
    import ParticipantTile from './ParticipantTile.svelte';
    import StreamTile from './StreamTile.svelte';
    import ParticipantMenu from './ParticipantMenu.svelte';
    import CallControls from './CallControls.svelte';
    import Icon from '../ui/Icon.svelte';

    export let title = 'Call';
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
    $: cols = tiles <= 1 ? 1 : tiles <= 4 ? 2 : tiles <= 9 ? 3 : 4;

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

<section class="relative flex flex-col w-full h-full min-h-0 bg-[#000] text-white select-none" aria-label="Call">
    <header class="h-12 shrink-0 flex items-center justify-between px-4 gap-3">
        <div class="flex items-center gap-2 min-w-0">
            <span class="w-2 h-2 rounded-full {s.inCall ? 'bg-[#23a55a]' : 'bg-[#f0b232] animate-pulse'}"></span>
            <span class="font-semibold truncate">{title}</span>
            <span class="text-sm text-[#949ba4] shrink-0">
                {#if s.inCall}{formatDuration($callDuration)} · {members.length} {members.length === 1 ? 'person' : 'people'}{:else}Connecting…{/if}
            </span>
            <span class="hidden sm:flex items-center gap-1 text-xs text-[#949ba4] ml-2" title="Media is end-to-end encrypted and relayed peer-to-peer">
                <Icon name="lock" size={13} /> End-to-end encrypted
            </span>
        </div>
        <div class="flex items-center gap-1">
            <button class="hdr-btn" title={maximized ? 'Show chat' : 'Focus on call'} aria-label={maximized ? 'Show chat' : 'Focus on call'} on:click={() => (maximized = !maximized)}>
                <Icon name={maximized ? 'collapse' : 'expand'} size={18} />
            </button>
            <button class="hdr-btn" title="Minimize call" aria-label="Minimize call" on:click={() => stageExpanded.set(false)}>
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
            <div class="grid gap-3 w-full h-full min-h-0 content-center" style="grid-template-columns:repeat({cols}, minmax(0, 1fr)); grid-auto-rows: minmax(0, {tiles <= 2 ? '1fr' : 'auto'});">
                {#each screens as st (st.streamId)}
                    <div class="min-h-0 {tiles <= 2 ? 'h-full' : 'aspect-video'}"><StreamTile stream={st} /></div>
                {/each}
                {#each members as peer (peer)}
                    <div class="min-h-0 {tiles <= 2 ? 'h-full' : 'aspect-video'}"><ParticipantTile {peer} camera={cameraOf[peer]} on:menu={openMenu} /></div>
                {/each}
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
