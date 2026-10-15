<script>
    import { onMount } from 'svelte';
    import { fade, scale, fly } from 'svelte/transition';
    import { backOut } from 'svelte/easing';
    import {
        isCallActive,
        peerInfo,
        incomingCall,
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
    import VideoView from '../call/VideoView.svelte';
    import { t } from '../../i18n/index.js';

    let pos = { x: 0, y: 0 };
    let drag = null;
    let moved = false;
    let miniH = 0;
    let winH = 800;
    let winW = 1280;

    // keep the card fully on screen when it grows (stream preview) or the window shrinks
    $: if (miniH && !drag) {
        pos = {
            x: Math.max(8, Math.min(pos.x, winW - 328)),
            y: Math.max(8, Math.min(pos.y, winH - miniH - 16)),
        };
    }

    onMount(() => {
        initCallEvents();
        pos = { x: window.innerWidth - 340, y: 68 }; // top-right, clear of the message composer
    });

    $: s = $callState;
    $: stageVisible = $view === 'chats' && $activeId === $peerInfo.chatId && $stageExpanded;
    $: showMini = $isCallActive && !stageVisible;
    $: focusStream = s.activeStreams.find((st) => st.streamId === (s.focusedStreamId || s.watching[0]) && st.mediaType !== 'camera');
    $: preview = focusStream ? callService.streamFor(focusStream.streamId, s, $videoStreams) : null;
    $: people = (s.members.length ? s.members : [s.self]).filter(Boolean).slice(0, 6);

    function onDown(e) {
        if (e.target.closest('button')) return;
        drag = { dx: e.clientX - pos.x, dy: e.clientY - pos.y };
        moved = false;
    }
    function onMove(e) {
        if (!drag) return;
        moved = true;
        pos = {
            x: Math.max(8, Math.min(e.clientX - drag.dx, window.innerWidth - 328)),
            y: Math.max(8, Math.min(e.clientY - drag.dy, window.innerHeight - miniH - 8)),
        };
    }
    function onUp() {
        if (drag && !moved) openStage();
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
    <div class="fixed inset-0 z-[10000] flex items-center justify-center backdrop-blur-md" style="background:var(--scrim)" transition:fade={{ duration: 200 }}>
        <div class="ring-card w-[320px] max-w-[calc(100vw-32px)] rounded-3xl p-7 flex flex-col items-center" transition:scale={{ duration: 320, start: 0.9, easing: backOut }} role="alertdialog" aria-label={$t('call.incoming')}>
            <div class="relative mb-5">
                <span class="absolute inset-0 rounded-full animate-ping" style="background:var(--ok-soft)"></span>
                <span class="absolute -inset-3 rounded-full ring-pulse"></span>
                <Avatar id={$incomingCall.from} name={$incomingCall.alias} size={96} />
            </div>
            <p class="text-xl font-bold truncate max-w-full">{$incomingCall.alias}</p>
            <p class="text-sm mt-1" style="color:var(--muted)">{$t('call.incomingDots')}</p>
            <div class="flex items-center gap-5 mt-7">
                <button class="round decline" title={$t('call.decline')} aria-label={$t('call.decline')} on:click={declineIncomingCall}>
                    <Icon name="hangup" size={26} />
                </button>
                <button class="round neutral" title={$t('call.acceptVideo')} aria-label={$t('call.acceptVideo')} on:click={() => accept(true)}>
                    <Icon name="video" size={24} />
                </button>
                <button class="round accept" title={$t('call.accept')} aria-label={$t('call.accept')} on:click={() => accept(false)}>
                    <Icon name="phone" size={24} />
                </button>
            </div>
        </div>
    </div>
{/if}

{#if $callError && !$isCallActive}
    <div class="toast fixed z-[10000] top-4 right-4 w-[320px] max-w-[calc(100vw-32px)] rounded-2xl p-3 text-sm flex gap-3" role="alert" transition:fly={{ y: -12, duration: 260 }}>
        <span class="font-semibold" style="color:var(--danger)">{$t('call.failed')}</span>
        <span class="flex-1" style="color:var(--muted)">{$callError}</span>
        <button class="dismiss" aria-label={$t('call.dismiss')} on:click={() => callState.update((c) => ({ ...c, error: null }))}><Icon name="close" size={16} /></button>
    </div>
{/if}

{#if showMini}
    <!-- svelte-ignore a11y-no-static-element-interactions -->
    <div
        class="mini-card fixed z-[9999] w-[320px] rounded-2xl overflow-hidden cursor-grab active:cursor-grabbing"
        class:dragging={!!drag}
        style="left:{pos.x}px;top:{pos.y}px"
        bind:offsetHeight={miniH}
        on:mousedown={onDown}
        transition:scale={{ duration: 260, start: 0.92, easing: backOut }}
    >
        {#if preview}
            <div class="relative aspect-video bg-black">
                <VideoView stream={preview} />
                <span class="absolute left-2 top-2 rounded bg-[#f23f43] px-1.5 py-0.5 text-[10px] font-extrabold uppercase">{$t('call.live')}</span>
            </div>
        {/if}
        <div class="px-3 pt-3 flex items-center gap-2">
            <span class="w-2 h-2 rounded-full {s.inCall ? '' : 'animate-pulse'}" style="background:{s.inCall ? 'var(--ok)' : 'var(--warn)'}"></span>
            <div class="min-w-0 flex-1">
                <p class="text-[13px] font-bold leading-tight" style="color:var(--ok)">{s.inCall ? $t('call.voiceConnected') : $t('call.connecting')}</p>
                <p class="text-xs truncate" style="color:var(--muted)">{$peerInfo.alias || $t('call.call')}{#if s.inCall} · {formatDuration($callDuration)}{/if}</p>
            </div>
            <div class="flex -space-x-2">
                {#each people as p (p)}
                    <Avatar id={p} name={$peerName(p)} size={26} speaking={!!s.speaking[p]} className="mini-av rounded-full" />
                {/each}
            </div>
        </div>
        <div class="p-3 flex items-center gap-2">
            <button class="mini {s.isMicMuted ? 'mini-off' : ''}" title={s.isMicMuted ? $t('call.unmute') : $t('call.mute')} aria-label={s.isMicMuted ? $t('call.unmute') : $t('call.mute')} on:click={toggleMute}>
                <Icon name={s.isMicMuted ? 'micOff' : 'mic'} size={19} />
            </button>
            <button class="mini {s.isDeafened ? 'mini-off' : ''}" title={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')} aria-label={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')} on:click={toggleDeafen}>
                <Icon name={s.isDeafened ? 'headphonesOff' : 'headphones'} size={19} />
            </button>
            <button class="mini {s.isScreenSharing ? 'mini-on' : ''}" title={$t('call.shareScreen')} aria-label={$t('call.shareScreen')} on:click={toggleScreenShare}>
                <Icon name="screenUp" size={19} />
            </button>
            <button class="mini" title={$t('call.openCall')} aria-label={$t('call.openCall')} on:click={openStage}>
                <Icon name="expand" size={18} />
            </button>
            <button class="mini leave flex-1" title={$t('call.leave')} aria-label={$t('call.leave')} on:click={endCall}>
                <Icon name="hangup" size={20} />
            </button>
        </div>
    </div>
{/if}

<style>
    .ring-card {
        background: var(--elevated);
        color: var(--text);
        border: 1px solid var(--line-soft);
        box-shadow: var(--shadow-lg);
    }
    .ring-pulse {
        border: 2px solid var(--ok);
        opacity: 0;
        animation: ring 1.6s var(--ease) infinite;
    }
    @keyframes ring {
        0% { transform: scale(0.85); opacity: 0.7; }
        100% { transform: scale(1.25); opacity: 0; }
    }
    .round {
        width: 60px;
        height: 60px;
        padding: 0;
        border: none;
        border-radius: 9999px;
        display: flex;
        align-items: center;
        justify-content: center;
        color: #fff;
    }
    .round:hover:not(:disabled) { transform: scale(1.08); border: none; }
    .round.decline, .round.decline:hover:not(:disabled) { background: var(--danger); }
    .round.accept, .round.accept:hover:not(:disabled) { background: var(--ok); }
    .round.accept { animation: nudge 1.4s var(--ease) infinite; }
    .round.neutral { background: var(--raised); color: var(--text); }
    .round.neutral:hover:not(:disabled) { background: var(--press); }
    @keyframes nudge {
        0%, 70%, 100% { transform: rotate(0); }
        75% { transform: rotate(-12deg); }
        85% { transform: rotate(10deg); }
        95% { transform: rotate(-4deg); }
    }
    .toast {
        background: var(--elevated);
        color: var(--text);
        border: 1px solid var(--line-soft);
        box-shadow: var(--shadow);
    }
    .dismiss { padding: 0; background: none; border: none; color: var(--faint); }
    .dismiss:hover:not(:disabled) { color: var(--text); background: none; border: none; }
    .mini-card {
        background: var(--glass);
        backdrop-filter: blur(20px) saturate(1.4);
        color: var(--text);
        border: 1px solid var(--line-soft);
        box-shadow: var(--shadow-lg);
        transition: box-shadow 0.2s, transform 0.25s var(--spring);
    }
    .mini-card.dragging { transform: scale(1.02); box-shadow: var(--shadow-lg), 0 0 0 2px var(--accent-ring); }
    :global(.mini-av) { box-shadow: 0 0 0 2px var(--panel); }
    .mini {
        width: 40px;
        height: 36px;
        padding: 0;
        border: none;
        border-radius: 11px;
        display: flex;
        align-items: center;
        justify-content: center;
        background: var(--raised);
        color: var(--text);
    }
    .mini:hover:not(:disabled) { background: var(--press); border: none; }
    .mini-off, .mini-off:hover:not(:disabled) { background: var(--danger-soft); color: var(--danger); }
    .mini-on, .mini-on:hover:not(:disabled) { background: var(--accent); color: #fff; }
    .mini.leave, .mini.leave:hover:not(:disabled) { background: var(--danger); color: #fff; }
</style>
