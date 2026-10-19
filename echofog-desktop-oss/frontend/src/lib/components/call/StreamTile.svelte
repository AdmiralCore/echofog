<script>
    import { callService, callState, videoStreams } from '../../services/call.service.js';
    import { peerName } from '../../stores/peers.store.js';
    import Icon from '../ui/Icon.svelte';
    import VideoView from './VideoView.svelte';
    import { t } from '../../i18n/index.js';

    export let stream; // { streamId, broadcaster, mediaType, width, height, fps, qualityId }
    export let focused = false;
    export let compact = false;

    let box;

    $: isOwn = stream.broadcaster === $callState.self;
    $: watching = isOwn || $callState.watching.includes(stream.streamId);
    $: media = callService.streamFor(stream.streamId, $callState, $videoStreams);
    $: name = $peerName(stream.broadcaster);
    $: label = qualityLabel(stream);
    $: speaking = !!$callState.speaking[stream.broadcaster];
    $: hasSound = !isOwn && !!callService.soundSlotOf(stream.streamId, $callState);
    $: soundOn = ($callState.streamVolume[stream.streamId] ?? 1) > 0;
    $: ownSound = isOwn && stream.streamId === $callState.screenStreamId && $callState.screenAudio;

    function qualityLabel(s) {
        if (!s.height) return '';
        return (s.height >= 2160 ? '4K' : s.height >= 1440 ? '1440p' : s.height + 'p') + (s.fps ? s.fps : '');
    }

    function fullscreen() {
        if (document.fullscreenElement) document.exitFullscreen();
        else box?.requestFullscreen?.().catch(() => {});
    }

    function toggleFocus() {
        callService.focusStream(focused ? null : stream.streamId);
    }
</script>

<div bind:this={box} class="group relative w-full h-full min-h-0 rounded-xl overflow-hidden bg-black" class:speaking>
    {#if watching && media}
        <VideoView stream={media} />
    {:else if watching}
        <div class="absolute inset-0 flex flex-col items-center justify-center gap-3 text-white/70 text-sm">
            <span class="w-6 h-6 rounded-full border-2 border-white/30 border-t-white animate-spin"></span>
            {$t('call.connectingStream')}
        </div>
    {:else}
        <div class="absolute inset-0 flex items-center justify-center bg-[#1e1f22]" style="background:radial-gradient(circle at 50% 40%, rgba(88,101,242,.18), transparent 70%), #1e1f22">
            <button class="watch h-9 px-4 border-0 rounded-full bg-white text-[#111214] text-sm font-bold flex items-center gap-2 shadow-lg hover:scale-[1.03] transition-transform" on:click={() => callService.watchStream(stream.streamId, !compact)}>
                <Icon name="eye" size={17} /> {$t('call.watch')}
            </button>
        </div>
    {/if}

    <div class="absolute left-2 top-2 flex items-center gap-1.5">
        <span class="rounded bg-[#f23f43] px-1.5 py-0.5 text-[11px] font-extrabold uppercase text-white tracking-wide">{$t('call.live')}</span>
        {#if label}<span class="rounded bg-black/60 px-1.5 py-0.5 text-[11px] font-bold text-white">{label}</span>{/if}
        {#if hasSound || ownSound}<span class="rounded bg-black/60 px-1 py-0.5 text-white" title={$t('call.streamHasSound')}><Icon name={hasSound && !soundOn ? 'volumeOff' : 'volume'} size={13} /></span>{/if}
    </div>
    <div class="absolute left-2 bottom-2 max-w-[70%] rounded-md bg-black/60 backdrop-blur px-2 py-1 text-[13px] font-semibold text-white truncate">
        {isOwn ? (stream.mediaType === 'camera' ? $t('call.yourCamera') : $t('call.yourScreen')) : name}
    </div>

    {#if watching}
        <div class="absolute right-2 top-2 flex items-center gap-1 opacity-0 group-hover:opacity-100 focus-within:opacity-100 transition-opacity">
            {#if hasSound}
                <button class="tile-btn" title={soundOn ? $t('call.streamSoundOff') : $t('call.streamSoundOn')} aria-label={soundOn ? $t('call.streamSoundOff') : $t('call.streamSoundOn')}
                    on:click={() => callService.setStreamVolume(stream.streamId, soundOn ? 0 : 1)}>
                    <Icon name={soundOn ? 'volume' : 'volumeOff'} size={17} />
                </button>
            {/if}
            {#if !isOwn}
                <button class="tile-btn" title={$t('call.switchRelayTitle')} aria-label={$t('call.switchRelay')} on:click={() => callService.switchRelay(stream.streamId)}>
                    <Icon name="relay" size={17} />
                </button>
                <button class="tile-btn" title={$t('call.stopWatching')} aria-label={$t('call.stopWatching')} on:click={() => callService.unwatchStream(stream.streamId)}>
                    <Icon name="eyeOff" size={17} />
                </button>
            {/if}
            <button class="tile-btn" title={focused ? $t('call.backToGrid') : $t('call.focus')} aria-label={focused ? $t('call.backToGrid') : $t('call.focus')} on:click={toggleFocus}>
                <Icon name={focused ? 'collapse' : 'expand'} size={17} />
            </button>
            <button class="tile-btn" title={$t('call.fullscreen')} aria-label={$t('call.fullscreen')} on:click={fullscreen}>
                <Icon name="fullscreen" size={17} />
            </button>
        </div>
    {/if}
</div>

<style>
    .watch:hover:not(:disabled) {
        background: #fff;
        border: none;
    }
    .tile-btn {
        width: 32px;
        height: 32px;
        border-radius: 8px;
        padding: 0;
        border: none;
        background: rgba(0, 0, 0, 0.6);
        color: #fff;
        display: flex;
        align-items: center;
        justify-content: center;
    }
    .tile-btn:hover:not(:disabled) {
        border: none;
        background: rgba(0, 0, 0, 0.85);
    }
    .speaking {
        box-shadow: inset 0 0 0 3px var(--color-success);
    }
</style>
