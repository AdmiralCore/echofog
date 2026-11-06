<script>
    import { onMount } from 'svelte';
    import { fade, scale, fly } from 'svelte/transition';
    
    import {
        isCallActive,
        peerInfo,
        waitingKey,
        incomingCall,
        callEnded,
        callError,
        callDuration,
        formatDuration,
        stageExpanded,
        initCallEvents,
        acceptIncomingCall,
        declineIncomingCall,
        toggleMute,
        toggleDeafen,
        toggleScreenShare,
        endCall,
    } from '../../stores/call.store.js';
    import { callService, callState, videoStreams } from '../../services/call.service.js';
    import { peerName } from '../../stores/peers.store.js';
    import { view } from '../../stores/nav.js';
    import { activeId, openChat } from '../../stores/chats.js';
    import Avatar from './Avatar.svelte';
    import Icon from './Icon.svelte';
    import IconButton from './IconButton.svelte';
    import Badge from './Badge.svelte';
    import VideoView from '../call/VideoView.svelte';
    import { t } from '../../i18n/index.js';

    // the card rests in one of the corners and never covers the message input. In a chat the corners
    // belong to the feed (under the header and the strips, above the input), by default the top right;
    // on the other pages they belong to the window, by default the bottom right. Each place remembers
    // its own corner. Dragging moves the card freely, releasing snaps it to the nearest corner.
    const KEYS = { chat: 'echofog.callMiniCornerChat', page: 'echofog.callMiniCorner' };
    const CORNERS = ['br', 'bl', 'tr', 'tl'];
    const load = (key, def) => { try { const c = localStorage.getItem(key); if (CORNERS.includes(c)) return c; } catch (_) { /* private mode */ } return def; };
    let corners = { chat: load(KEYS.chat, 'tr'), page: load(KEYS.page, 'br') };
    let pos = { x: 0, y: 0 };
    let drag = null;
    let moved = false;
    let miniH = 0;
    let miniW = 320;
    let winH = 800;
    let winW = 1280;
    let card;
    let area = { left: 0, top: 0, right: 1280, bottom: 800 };
    const GAP = 16;

    // the free area: the whole window (minus the rail on the left), or in a chat the feed above the input
    function measure() {
        const rail = winW < 640 ? 0 : 56;
        let a = { left: rail, top: 0, right: winW, bottom: winH - (winW < 640 ? 56 : 0) };
        if ($view === 'chats') {
            const feed = document.querySelector('.main .messages');
            if (feed) {
                const r = feed.getBoundingClientRect();
                const inputs = [...document.querySelectorAll('.composer:not(.thread-composer)')].map((e) => e.getBoundingClientRect().top).filter((y) => y > r.top);
                a = { left: r.left, top: r.top, right: r.right, bottom: Math.min(r.bottom, ...inputs) };
            }
        }
        if (a.left !== area.left || a.top !== area.top || a.right !== area.right || a.bottom !== area.bottom) area = a;
    }
    let timer;
    onMount(() => { initCallEvents(); timer = setInterval(measure, 250); return () => clearInterval(timer); });

    $: place = $view === 'chats' ? 'chat' : 'page';
    $: corner = corners[place];
    $: winW, winH, $view, showMini, measure();
    $: cornerStyle = (() => {
        const y = corner[0] === 'b' ? `top:${Math.max(area.top + 8, area.bottom - miniH - GAP)}px;` : `top:${area.top + (place === 'chat' ? 8 : GAP)}px;`;
        const x = corner[1] === 'r' ? `left:${Math.max(area.left + 8, area.right - miniW - GAP)}px;` : `left:${area.left + GAP}px;`;
        return y + x;
    })();

    $: s = $callState;
    $: stageVisible = $view === 'chats' && $activeId === $peerInfo.chatId && $stageExpanded;
    $: showMini = $isCallActive && !stageVisible;
    $: focusStream = s.activeStreams.find((st) => st.streamId === (s.focusedStreamId || s.watching[0]) && st.mediaType !== 'camera');
    $: preview = focusStream ? callService.streamFor(focusStream.streamId, s, $videoStreams) : null;
    $: people = (s.members.length ? s.members : [s.self]).filter(Boolean).slice(0, 6);

    function onDown(e) {
        if (e.target.closest('button')) return;
        const r = card.getBoundingClientRect();
        drag = { dx: e.clientX - r.left, dy: e.clientY - r.top };
        pos = { x: r.left, y: r.top };
        moved = false;
    }
    function onMove(e) {
        if (!drag) return;
        moved = true;
        pos = {
            x: Math.max(8, Math.min(e.clientX - drag.dx, winW - miniW - 8)),
            y: Math.max(8, Math.min(e.clientY - drag.dy, winH - miniH - 8)),
        };
    }
    function onUp() {
        if (drag && !moved) openStage();
        else if (drag) {
            measure();
            const c = (pos.y + miniH / 2 > (area.top + area.bottom) / 2 ? 'b' : 't') + (pos.x + miniW / 2 > (area.left + area.right) / 2 ? 'r' : 'l');
            corners = { ...corners, [place]: c };
            try { localStorage.setItem(KEYS[place], c); } catch (_) { /* private mode */ }
        }
        drag = null;
    }
    // answering takes you to the caller's chat, where the call stage lives
    async function accept(withVideo) {
        const from = $incomingCall?.from;
        if (from) {
            openChat($incomingCall.chatId || 'd:' + from); // a group call opens its group
            view.set('chats');
        }
        await acceptIncomingCall(withVideo);
    }

    function openStage() {
        if ($peerInfo.chatId) openChat($peerInfo.chatId);
        stageExpanded.set(true);
        view.set('chats');
    }
</script>

<svelte:window on:mousemove={onMove} on:mouseup={onUp} bind:innerHeight={winH} bind:innerWidth={winW} />

{#if $incomingCall}
    <div class="ring-layer" transition:fade={{ duration: 160 }}>
        <div class="ring-card" transition:scale={{ duration: 200, start: 0.94 }} role="alertdialog" aria-label={$t('call.incoming')}>
            <div class="ring-av">
                <span class="ring-pulse"></span>
                <Avatar id={$incomingCall.from} name={$incomingCall.alias} size={88} />
            </div>
            <p class="ring-name">{$incomingCall.alias}</p>
            <p class="ring-sub">{$t('call.incomingDots')}</p>
            <div class="ring-btns">
                <span class="rb"><button class="round decline" title={$t('call.decline')} aria-label={$t('call.decline')} on:click={declineIncomingCall}>
                    <Icon name="hangup" size={24} />
                </button><span>{$t('call.decline')}</span></span>
                <span class="rb"><button class="round neutral" title={$t('call.acceptVideo')} aria-label={$t('call.acceptVideo')} on:click={() => accept(true)}>
                    <Icon name="video" size={22} />
                </button><span>{$t('call.acceptVideo')}</span></span>
                <span class="rb"><button class="round accept" title={$t('call.accept')} aria-label={$t('call.accept')} on:click={() => accept(false)}>
                    <Icon name="phone" size={22} />
                </button><span>{$t('call.accept')}</span></span>
            </div>
        </div>
    </div>
{/if}

{#if $callEnded && !$isCallActive && !$callError}
    <div class="toast" role="status" transition:fly={{ y: -12, duration: 200 }}>
        <span class="t-ic"><Icon name="phone" size={16} /></span>
        <div class="t-text"><b>{$t('call.ended')}</b><span>{$callEnded}</span></div>
        <IconButton size="sm" icon="close" label={$t('call.dismiss')} on:click={() => callEnded.set('')} />
    </div>
{/if}

{#if $callError && !$isCallActive}
    <div class="toast err" role="alert" transition:fly={{ y: -12, duration: 200 }}>
        <span class="t-ic"><Icon name="warning" size={16} /></span>
        <div class="t-text"><b>{$t('call.failed')}</b><span>{$callError}</span></div>
        <IconButton size="sm" icon="close" label={$t('call.dismiss')} on:click={() => callState.update((c) => ({ ...c, error: null }))} />
    </div>
{/if}

{#if showMini}
    <!-- svelte-ignore a11y-no-static-element-interactions -->
    <div
        class="mini-card"
        class:dragging={!!drag}
        style={drag ? `left:${pos.x}px;top:${pos.y}px` : cornerStyle}
        bind:this={card}
        bind:offsetHeight={miniH}
        bind:offsetWidth={miniW}
        on:mousedown={onDown}
        transition:scale={{ duration: 180, start: 0.94 }}
    >
        {#if preview}
            <div class="mini-pic">
                <VideoView stream={preview} />
                <span class="mini-live"><Badge kind="live" size="sm" /></span>
            </div>
        {/if}
        <div class="mini-head">
            <span class="st-dot" class:wait={!s.inCall}></span>
            <div class="mini-text">
                <p class="st">{s.inCall ? $t('call.voiceConnected') : $t(waitingKey(s))}</p>
                <p class="who">{$peerInfo.alias || $t('call.call')}{#if s.inCall}{' · '}{formatDuration($callDuration)}{/if}</p>
            </div>
            <div class="faces">
                {#each people as p (p)}
                    <Avatar id={p} name={$peerName(p)} size={26} speaking={!!s.speaking[p]} className="mini-av rounded-full" />
                {/each}
            </div>
        </div>
        <div class="mini-btns">
            <button class="mini {s.isMicMuted ? 'mini-off' : ''}" title={s.isMicMuted ? $t('call.unmute') : $t('call.mute')} aria-label={s.isMicMuted ? $t('call.unmute') : $t('call.mute')} aria-pressed={s.isMicMuted} on:click={toggleMute}>
                <Icon name={s.isMicMuted ? 'micOff' : 'mic'} size={18} />
            </button>
            <button class="mini {s.isDeafened ? 'mini-off' : ''}" title={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')} aria-label={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')} aria-pressed={s.isDeafened} on:click={toggleDeafen}>
                <Icon name={s.isDeafened ? 'headphonesOff' : 'headphones'} size={18} />
            </button>
            <button class="mini {s.isScreenSharing ? 'mini-on' : ''}" title={$t('call.shareScreen')} aria-label={$t('call.shareScreen')} aria-pressed={s.isScreenSharing} on:click={toggleScreenShare}>
                <Icon name="screenUp" size={18} />
            </button>
            <button class="mini" title={$t('call.openCall')} aria-label={$t('call.openCall')} on:click={openStage}>
                <Icon name="expand" size={17} />
            </button>
            <button class="mini leave grow" title={$t('call.leave')} aria-label={$t('call.leave')} on:click={endCall}>
                <Icon name="hangup" size={19} />
            </button>
        </div>
    </div>
{/if}

<style>
    .ring-layer { position: fixed; inset: 0; z-index: 10000; display: flex; align-items: center; justify-content: center; background: var(--scrim); }
    .ring-card {
        width: 320px; max-width: calc(100vw - 32px); padding: var(--sp-6) var(--sp-6) var(--sp-5);
        display: flex; flex-direction: column; align-items: center; text-align: center;
        background: var(--surface-1); color: var(--text); border: 1px solid var(--border-strong);
        border-radius: var(--r-xl); box-shadow: var(--shadow-xl);
    }
    .ring-av { position: relative; margin-bottom: var(--sp-4); }
    .ring-pulse { position: absolute; inset: -6px; border-radius: 50%; border: 2px solid var(--ok); opacity: 0; animation: ring 1.6s var(--ease) infinite; }
    @keyframes ring {
        0% { transform: scale(0.9); opacity: 0.7; }
        100% { transform: scale(1.22); opacity: 0; }
    }
    .ring-name { font-size: var(--fs-xl); line-height: var(--lh-xl); font-weight: var(--fw-bold); max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .ring-sub { margin-top: 2px; font-size: var(--fs-sm); color: var(--muted); }
    .ring-btns { display: flex; align-items: flex-start; justify-content: center; gap: var(--sp-4); margin-top: var(--sp-6); }
    .rb { display: flex; flex-direction: column; align-items: center; gap: 6px; width: 84px; font-size: var(--fs-xs); color: var(--muted); line-height: 1.25; }
    .round {
        width: 56px; height: 56px; padding: 0; border: none; border-radius: 50%;
        display: flex; align-items: center; justify-content: center; color: #fff;
        transition: background var(--dur) var(--ease), transform var(--dur) var(--ease);
    }
    .round:hover:not(:disabled) { border: none; transform: translateY(-1px); }
    .round:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 2px; }
    .round.decline, .round.decline:hover:not(:disabled) { background: var(--danger); color: var(--danger-ink); }
    .round.accept, .round.accept:hover:not(:disabled) { background: var(--ok); color: #fff; }
    .round.neutral, .round.neutral:hover:not(:disabled) { background: var(--surface-3); color: var(--text); }
    .round.neutral:hover:not(:disabled) { background: var(--hover); }
    @media (prefers-reduced-motion: reduce) { .ring-pulse { animation: none; opacity: 0.5; } }

    .toast {
        position: fixed; z-index: 10000; top: var(--sp-4); right: var(--sp-4);
        width: 340px; max-width: calc(100vw - 32px); display: flex; align-items: flex-start; gap: var(--sp-3); padding: 10px 8px 10px 12px;
        background: var(--surface-3); color: var(--text); border: 1px solid var(--border-strong); border-radius: var(--r-lg); box-shadow: var(--shadow-lg);
    }
    .t-ic { margin-top: 1px; color: var(--info); display: inline-flex; }
    .toast.err .t-ic { color: var(--danger-fg); }
    .t-text { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; font-size: var(--fs-sm); line-height: var(--lh-sm); }
    .t-text span { color: var(--muted); }
    .toast.err .t-text b { color: var(--danger-fg); }

    .mini-card {
        position: fixed; z-index: 9999; width: 320px; overflow: hidden; border-radius: var(--r-xl); cursor: grab;
        background: var(--surface-2); color: var(--text); border: 1px solid var(--border-strong); box-shadow: var(--shadow-lg);
        transition: box-shadow var(--dur) var(--ease);
    }
    .mini-card:active { cursor: grabbing; }
    .mini-card.dragging { box-shadow: var(--shadow-lg), 0 0 0 2px var(--accent-ring); }
    .mini-pic { position: relative; aspect-ratio: 16 / 9; background: var(--stage); }
    .mini-live { position: absolute; left: 8px; top: 8px; }
    .mini-head { display: flex; align-items: center; gap: var(--sp-2); padding: var(--sp-3) var(--sp-3) 0; }
    .st-dot { width: 8px; height: 8px; border-radius: 50%; flex: none; background: var(--ok); }
    .st-dot.wait { background: var(--warn); animation: pulse-dot 1.4s var(--ease) infinite; }
    @keyframes pulse-dot { 50% { opacity: 0.4; } }
    .mini-text { min-width: 0; flex: 1; }
    .mini-text .st { font-size: var(--fs-sm); font-weight: var(--fw-bold); line-height: 1.2; color: var(--ok-fg); }
    .mini-text .who { font-size: var(--fs-xs); color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .faces { display: flex; }
    .faces > :global(* + *) { margin-left: -8px; }
    :global(.mini-av) { box-shadow: 0 0 0 2px var(--surface-2); }
    .mini-btns { display: flex; align-items: center; gap: var(--sp-2); padding: var(--sp-3); }
    .mini {
        width: 40px; height: var(--ctl-lg, 40px); padding: 0; border: none; border-radius: var(--r-md);
        display: flex; align-items: center; justify-content: center; background: var(--surface-3); color: var(--text);
    }
    .mini:hover:not(:disabled) { background: var(--hover); border: none; }
    .mini:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 1px; }
    .mini-off, .mini-off:hover:not(:disabled) { background: var(--danger-soft); color: var(--danger-fg); }
    .mini-on, .mini-on:hover:not(:disabled) { background: var(--accent); color: var(--accent-ink); }
    .mini.leave, .mini.leave:hover:not(:disabled) { background: var(--danger); color: var(--danger-ink); }
    .mini.grow { flex: 1; width: auto; }
</style>
