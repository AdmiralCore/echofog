<script>
    import { createEventDispatcher } from 'svelte';
    import { fade } from 'svelte/transition';
    import { peerInfo } from '../../stores/call.store.js';
    import { callService, callState, videoStreams } from '../../services/call.service.js';
    import { callDuration, formatDuration, stageExpanded, waitingKey, toggleMute } from '../../stores/call.store.js';
    import ParticipantTile from './ParticipantTile.svelte';
    import StreamTile from './StreamTile.svelte';
    import ParticipantMenu from './ParticipantMenu.svelte';
    import CallControls from './CallControls.svelte';
    import CallPathBadge from './CallPathBadge.svelte';
    import Icon from '../ui/Icon.svelte';
    import IconButton from '../ui/IconButton.svelte';
    import Badge from '../ui/Badge.svelte';
    import Menu from '../ui/Menu.svelte';
    import './call-icons.js';
    import './call-ui.css';
    import { t } from '../../i18n/index.js';

    export let title = '';
    /** maximized ("theatre"): the stage takes the whole chat column */
    export let maximized = false;
    /** the chat beside the stage is shown (the button in the head opens and closes it) */
    export let chatOpen = false;
    /** unread messages of the room chat while it is closed (a count on the chat button) */
    export let unread = 0;
    /** a narrow window: the space's channels open from a button in the head */
    export let channelsButton = false;

    const dispatch = createEventDispatcher();

    // how the room is laid out is remembered on this device: tiles in a grid, or the stream big
    // with the people in a strip beside it / under it; the strip can fold to faces only
    const LS = 'echofog.call.layout';
    let layout = 'side'; // grid | side | bottom
    let railPref = null; // null: the column folds by itself when the picture would get narrower than PIC_MIN
    try {
        const saved = JSON.parse(localStorage.getItem(LS) || '{}');
        if (['grid', 'side', 'bottom'].includes(saved.layout)) layout = saved.layout;
        if (typeof saved.railPref === 'boolean') railPref = saved.railPref;
    } catch (_) {}
    function remember() {
        try { localStorage.setItem(LS, JSON.stringify({ layout, railPref })); } catch (_) {}
    }
    function setLayout(l) {
        layout = l;
        if (l === 'grid') callService.focusStream(null);
        remember();
    }
    function foldRail() {
        railPref = !railFolded;
        remember();
    }

    let menu = null; // { peer, x, y }
    let root;
    let stageW = 0;
    const PIC_MIN = 1000; // the picture is not made narrower than this by the people beside it

    $: s = $callState;
    $: members = orderMembers(s.members.length ? s.members : s.self ? [s.self] : [], s.self);
    // Camera streams render inside their owner's tile; screens get their own tiles.
    $: cameraOf = cameraMap(s, $videoStreams);
    $: screens = s.activeStreams.filter((st) => st.mediaType !== 'camera');
    $: focused = s.focusedStreamId ? s.activeStreams.find((st) => st.streamId === s.focusedStreamId) : null;
    // the big picture: the one focused by hand, else the first screen when the layout asks for one
    $: main = focused || (layout !== 'grid' ? screens[0] : null);
    $: others = main ? screens.filter((st) => st.streamId !== main.streamId) : [];
    $: tiles = screens.length + members.length;
    $: rail = layout === 'bottom' ? 'bottom' : 'side';
    // the people beside the picture: full tiles while the picture keeps PIC_MIN, else faces only
    $: railFolded = railPref ?? (stageW > 0 && stageW - 32 - 176 - 12 < PIC_MIN);
    $: isRoom = !!$peerInfo.room;
    $: tight = stageW > 0 && stageW < 720; // the layout buttons move into More
    $: wide = stageW > 1000; // below this the badges that the video tags repeat are left out of the header
    $: qualityBadge = main ? qualityLabel(main) : screens[0] ? qualityLabel(screens[0]) : '';
    $: anyLive = screens.length > 0;

    // grid: every tile is the same 16:9 box; the column count is whichever makes the boxes biggest
    const GAP = 8;
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

    function qualityLabel(st) {
        if (!st.height) return '';
        return (st.height >= 2160 ? '4K' : st.height >= 1440 ? '1440p' : st.height + 'p') + (st.fps ? st.fps : '');
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

    // "More": the people column, the microphone, theatre and full screen with their keys
    $: moreItems = [
        ...(tight ? [
            { id: 'l:grid', icon: 'layout', label: $t('call.layout.grid') },
            { id: 'l:side', icon: 'panel', label: $t('call.layout.side') },
            { id: 'l:bottom', icon: 'panelBottom', label: $t('call.layout.bottom') },
            { separator: true },
        ] : []),
        ...(main && rail === 'side' ? [{ id: 'rail', icon: railFolded ? 'chevronLeft' : 'chevronRight', label: railFolded ? $t('call.rail.unfold') : $t('call.rail.fold') }] : []),
        { id: 'mic', icon: s.isMicMuted ? 'micOff' : 'mic', label: s.isMicMuted ? $t('call.unmute') : $t('call.mute'), hint: 'M' },
        { id: 'theatre', icon: maximized ? 'collapse' : 'theater', label: maximized ? $t('call.showChat') : $t('call.theater'), hint: 'T' },
        { id: 'full', icon: 'fullscreen', label: $t('call.fullscreen'), hint: 'F' },
    ];
    function onMore(e) {
        if (String(e.detail).startsWith('l:')) setLayout(e.detail.slice(2));
        else if (e.detail === 'rail') foldRail();
        else if (e.detail === 'mic') toggleMute();
        else if (e.detail === 'theatre') maximized = !maximized;
        else if (e.detail === 'full') fullscreenMain();
    }

    function openMenu(e) {
        menu = e.detail;
    }

    function fullscreenMain() {
        if (document.fullscreenElement) return document.exitFullscreen();
        const el = root?.querySelector('.vframe[data-main-frame]') || root?.querySelector('.vframe') || root;
        el?.requestFullscreen?.().catch(() => {});
    }

    // keyboard: Esc closes a menu, then leaves the theatre; F full screen; T theatre; M microphone
    function onKey(e) {
        if (e.ctrlKey || e.metaKey || e.altKey) return;
        const el = e.target;
        if (el?.closest?.('input, textarea, select, [contenteditable="true"], [role="dialog"]')) return;
        if (e.key === 'Escape') {
            if (menu) menu = null;
            else if (maximized) maximized = false;
            else return;
            e.preventDefault();
        } else if (e.key === 'f' || e.key === 'F' || e.key === 'а' || e.key === 'А') {
            e.preventDefault();
            fullscreenMain();
        } else if (e.key === 't' || e.key === 'T' || e.key === 'е' || e.key === 'Е') {
            e.preventDefault();
            maximized = !maximized;
        } else if (e.key === 'm' || e.key === 'M' || e.key === 'ь' || e.key === 'Ь') {
            e.preventDefault();
            toggleMute();
        }
    }
</script>

<svelte:window on:keydown={onKey} />

<section bind:this={root} bind:clientWidth={stageW} class="call-stage relative flex flex-col w-full h-full min-h-0 select-none" aria-label={$t('call.call')}>
    <header class="head shrink-0 flex items-center justify-between px-4 gap-3 border-b" style="border-color:var(--border)">
        <div class="flex items-center gap-2.5 min-w-0">
            {#if channelsButton}<IconButton size="sm" icon="panel" label={$t('chats.channels')} on:click={() => dispatch('channels')} />{/if}
            <span class="head-ic shrink-0 {s.inCall ? '' : 'animate-pulse'}" class:wait={!s.inCall}><Icon name={isRoom ? 'volume' : 'phone'} size={17} /></span>
            <span class="font-bold truncate text-fg" style="font-size:var(--fs-lg)">{title || $t('call.call')}</span>
            <span class="shrink-0 tabular-nums" style="font-size:var(--fs-sm);color:var(--muted)">
                {#if s.inCall}{formatDuration($callDuration)} · {$t('call.people', { n: members.length })}{:else}{$t(waitingKey(s))}{/if}
            </span>
            <span class="e2e shrink-0" title={$t('call.e2eTitle')}>
                <Icon name="lock" size={13} />{#if wide}<span>{$t('call.e2e')}</span>{/if}
            </span>
            <CallPathBadge />
            {#if anyLive && wide}<Badge kind="live" size="sm" />{/if}
            {#if qualityBadge && wide}<Badge kind="quality" size="sm">{qualityBadge}</Badge>{/if}
        </div>
        <div class="flex items-center gap-0.5 shrink-0">
            {#if !tight}<div class="seg" role="group" aria-label={$t('call.layout')}>
                <IconButton size="sm" icon="layout" label={$t('call.layout.grid')} pressed={layout === 'grid' || !main} on:click={() => setLayout('grid')} />
                <IconButton size="sm" icon="panel" label={$t('call.layout.side')} pressed={layout === 'side' && !!main} on:click={() => setLayout('side')} />
                <IconButton size="sm" icon="panelBottom" label={$t('call.layout.bottom')} pressed={layout === 'bottom' && !!main} on:click={() => setLayout('bottom')} />
            </div>{/if}
            <IconButton size="sm" icon="message" label={chatOpen ? $t('call.chatHide') : $t('call.chatShow')} pressed={chatOpen} badge={chatOpen ? 0 : unread} on:click={() => dispatch('togglechat')} />
            <IconButton size="sm" icon={maximized ? 'collapse' : 'theater'} label="{maximized ? $t('call.showChat') : $t('call.theater')} (T)" pressed={maximized} on:click={() => (maximized = !maximized)} />
            <IconButton size="sm" icon="fullscreen" label="{$t('call.fullscreen')} (F)" on:click={fullscreenMain} />
            <Menu align="end" width={300} label={$t('call.more')} items={moreItems} on:select={onMore}>
                <IconButton slot="trigger" let:toggle let:open size="sm" icon="moreH" label={$t('call.more')} pressed={open} on:click={toggle} />
            </Menu>
            <IconButton size="sm" icon="chevronUp" label={$t('call.minimize')} on:click={() => stageExpanded.set(false)} />
        </div>
    </header>

    <div class="flex-1 min-h-0 p-4">
        {#if main}
            <div class="flex w-full h-full min-h-0 gap-2.5" class:flex-col={rail === 'bottom'}>
                <div class="pic flex-1 min-w-0 min-h-0">
                    <div class="pic-box"><StreamTile stream={main} focused camera={cameraOf[main.broadcaster]} /></div>
                </div>
                {#if rail === 'side'}
                    {#if railFolded}
                        <div class="strip shrink-0 flex flex-col items-center gap-1.5 overflow-y-auto" style="width:var(--participants-w-strip)">
                            {#each others as st (st.streamId)}
                                <button class="vbtn strip-stream" title={$t('call.focus')} aria-label={$t('call.focus')} on:click={() => callService.focusStream(st.streamId)}><Icon name="monitor" size={16} /></button>
                            {/each}
                            {#each members as peer (peer)}
                                <div class="shrink-0" style="width:40px;height:40px"><ParticipantTile {peer} camera={cameraOf[peer]} strip on:menu={openMenu} /></div>
                            {/each}
                        </div>
                    {:else}
                        <div class="shrink-0 flex flex-col gap-2.5 overflow-y-auto" style="width:var(--participants-w)">
                            {#each others as st (st.streamId)}
                                <div class="aspect-video shrink-0"><StreamTile stream={st} compact /></div>
                            {/each}
                            {#each members as peer (peer)}
                                <div class="shrink-0" style="aspect-ratio:5/4"><ParticipantTile {peer} camera={cameraOf[peer]} compact on:menu={openMenu} /></div>
                            {/each}
                        </div>
                    {/if}
                {:else}
                    <div class="shrink-0 flex gap-2 overflow-x-auto pb-0.5" style="height:{railFolded ? 56 : 96}px">
                        {#each others as st (st.streamId)}
                            <div class="aspect-video shrink-0 h-full"><StreamTile stream={st} compact /></div>
                        {/each}
                        {#each members as peer (peer)}
                            <div class="aspect-video shrink-0 h-full"><ParticipantTile {peer} camera={cameraOf[peer]} compact on:menu={openMenu} /></div>
                        {/each}
                    </div>
                {/if}
            </div>
        {:else}
            <div class="relative w-full h-full min-h-0 overflow-hidden" bind:clientWidth={gridW} bind:clientHeight={gridH}>
                <div class="absolute inset-0 flex flex-wrap content-center justify-center" style="gap:{GAP}px">
                    {#each screens as st (st.streamId)}
                        <div class="shrink-0" style="width:{tile.w}px;height:{tile.h}px"><StreamTile stream={st} camera={cameraOf[st.broadcaster]} /></div>
                    {/each}
                    {#each members as peer (peer)}
                        <div class="shrink-0" style="width:{tile.w}px;height:{tile.h}px"><ParticipantTile {peer} camera={cameraOf[peer]} on:menu={openMenu} /></div>
                    {/each}
                </div>
            </div>
        {/if}
    </div>

    <footer class="shrink-0 flex justify-center pb-4 pt-1">
        <CallControls />
    </footer>

    {#if s.notice || s.streamError}
        <div class="notice absolute left-1/2 -translate-x-1/2 bottom-16 max-w-[90%]" class:err={!!s.streamError} role="status" transition:fade={{ duration: 150 }}>
            {s.streamError || s.notice}
        </div>
    {/if}

    {#if menu}
        <ParticipantMenu peer={menu.peer} x={menu.x} y={menu.y} on:close={() => (menu = null)} />
    {/if}
</section>

<style>
    .call-stage { background: var(--stage); color: var(--text); }
    :global(.call-stage:fullscreen) { background: #000; }
    .head { height: var(--top-h, 56px); }
    .head-ic { width: 28px; height: 28px; border-radius: 50%; display: grid; place-items: center; color: var(--muted); }
    .head-ic.wait { color: var(--warn-fg); }
    /* the picture: a 16:9 box as large as the area allows, centred; the stage colour shows round it */
    .pic { container-type: size; display: flex; align-items: center; justify-content: center; }
    .pic-box { width: min(100cqw, calc(100cqh * 16 / 9)); aspect-ratio: 16 / 9; }
    .e2e { display: inline-flex; align-items: center; gap: 4px; font-size: var(--fs-xs); color: var(--faint); }
    .seg { display: flex; gap: 2px; padding: 2px; margin-right: 4px; border-radius: var(--r-md); background: var(--surface-2); }
    .strip-stream { background: var(--surface-3); color: var(--text); flex: none; width: 40px; height: 40px; }
    .notice {
        padding: 8px 12px; border-radius: var(--r-lg); font-size: var(--fs-sm); background: var(--surface-3);
        border: 1px solid var(--border-strong); color: var(--text); box-shadow: var(--shadow-lg);
    }
    .notice.err { color: var(--danger-fg); }
</style>
