<script>
    import { onMount } from 'svelte';
    import { fade, scale } from 'svelte/transition';
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
    <div class="fixed inset-0 z-[10000] flex items-center justify-center bg-black/60 backdrop-blur-sm" transition:fade={{ duration: 150 }}>
        <div class="w-[320px] rounded-3xl bg-[#111214] border border-white/10 shadow-2xl p-7 flex flex-col items-center text-white" transition:scale={{ duration: 180, start: 0.94 }} role="alertdialog" aria-label="Incoming call">
            <div class="relative mb-5">
                <span class="absolute inset-0 rounded-full bg-[#23a55a]/30 animate-ping"></span>
                <Avatar id={$incomingCall.from} name={$incomingCall.alias} size={96} />
            </div>
            <p class="text-xl font-bold truncate max-w-full">{$incomingCall.alias}</p>
            <p class="text-sm text-[#949ba4] mt-1">Incoming call…</p>
            <div class="flex items-center gap-5 mt-7">
                <button class="round bg-[#da373c] hover:bg-[#a12828]" title="Decline" aria-label="Decline" on:click={declineIncomingCall}>
                    <Icon name="hangup" size={26} />
                </button>
                <button class="round bg-[#2b2d31] hover:bg-[#35373c]" title="Accept with video" aria-label="Accept with video" on:click={() => accept(true)}>
                    <Icon name="video" size={24} />
                </button>
                <button class="round bg-[#23a55a] hover:bg-[#1a8346]" title="Accept" aria-label="Accept" on:click={() => accept(false)}>
                    <Icon name="phone" size={24} />
                </button>
            </div>
        </div>
    </div>
{/if}

{#if $callError && !$isCallActive}
    <div class="fixed z-[10000] top-4 right-4 w-[320px] rounded-xl bg-[#111214] border border-white/10 p-3 shadow-xl text-sm text-white flex gap-3" role="alert" transition:fade={{ duration: 150 }}>
        <span class="text-[#f23f43] font-semibold">Call failed</span>
        <span class="flex-1 text-[#b5bac1]">{$callError}</span>
        <button class="text-[#949ba4] hover:text-white" aria-label="Dismiss" on:click={() => callState.update((c) => ({ ...c, error: null }))}><Icon name="close" size={16} /></button>
    </div>
{/if}

{#if showMini}
    <!-- svelte-ignore a11y-no-static-element-interactions -->
    <div
        class="fixed z-[9999] w-[320px] rounded-2xl overflow-hidden bg-[#111214] border border-white/10 shadow-2xl text-white cursor-grab active:cursor-grabbing"
        style="left:{pos.x}px;top:{pos.y}px"
        bind:offsetHeight={miniH}
        on:mousedown={onDown}
        transition:fade={{ duration: 150 }}
    >
        {#if preview}
            <div class="relative aspect-video bg-black">
                <VideoView stream={preview} />
                <span class="absolute left-2 top-2 rounded bg-[#f23f43] px-1.5 py-0.5 text-[10px] font-extrabold uppercase">Live</span>
            </div>
        {/if}
        <div class="px-3 pt-3 flex items-center gap-2">
            <span class="w-2 h-2 rounded-full {s.inCall ? 'bg-[#23a55a]' : 'bg-[#f0b232] animate-pulse'}"></span>
            <div class="min-w-0 flex-1">
                <p class="text-[13px] font-bold text-[#23a55a] leading-tight">{s.inCall ? 'Voice connected' : 'Connecting…'}</p>
                <p class="text-xs text-[#949ba4] truncate">{$peerInfo.alias || 'Call'}{#if s.inCall} · {formatDuration($callDuration)}{/if}</p>
            </div>
            <div class="flex -space-x-2">
                {#each people as p (p)}
                    <Avatar id={p} name={$peerName(p)} size={26} speaking={!!s.speaking[p]} className="ring-2 ring-[#111214] rounded-full" />
                {/each}
            </div>
        </div>
        <div class="p-3 flex items-center gap-2">
            <button class="mini {s.isMicMuted ? 'mini-off' : ''}" title={s.isMicMuted ? 'Unmute' : 'Mute'} aria-label={s.isMicMuted ? 'Unmute' : 'Mute'} on:click={toggleMute}>
                <Icon name={s.isMicMuted ? 'micOff' : 'mic'} size={19} />
            </button>
            <button class="mini {s.isDeafened ? 'mini-off' : ''}" title={s.isDeafened ? 'Undeafen' : 'Deafen'} aria-label={s.isDeafened ? 'Undeafen' : 'Deafen'} on:click={toggleDeafen}>
                <Icon name={s.isDeafened ? 'headphonesOff' : 'headphones'} size={19} />
            </button>
            <button class="mini {s.isScreenSharing ? 'mini-on' : ''}" title="Share screen" aria-label="Share screen" on:click={toggleScreenShare}>
                <Icon name="screenUp" size={19} />
            </button>
            <button class="mini" title="Open call" aria-label="Open call" on:click={openStage}>
                <Icon name="expand" size={18} />
            </button>
            <button class="mini flex-1 !bg-[#da373c] hover:!bg-[#a12828] !text-white" title="Leave call" aria-label="Leave call" on:click={endCall}>
                <Icon name="hangup" size={20} />
            </button>
        </div>
    </div>
{/if}

<style>
    .round {
        width: 60px;
        height: 60px;
        border-radius: 9999px;
        display: flex;
        align-items: center;
        justify-content: center;
        color: #fff;
        transition: background 0.12s, transform 0.12s;
    }
    .round:hover {
        transform: scale(1.05);
    }
    .mini {
        width: 40px;
        height: 36px;
        border-radius: 10px;
        display: flex;
        align-items: center;
        justify-content: center;
        background: #2b2d31;
        color: #dbdee1;
    }
    .mini:hover {
        background: #35373c;
        color: #fff;
    }
    .mini-off {
        background: rgba(242, 63, 67, 0.15);
        color: #f23f43;
    }
    .mini-on {
        background: #fff;
        color: #111214;
    }
</style>
