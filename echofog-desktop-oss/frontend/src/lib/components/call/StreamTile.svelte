<script>
    import { callService, callState, videoStreams } from '../../services/call.service.js';
    import { peerName } from '../../stores/peers.store.js';
    import Icon from '../ui/Icon.svelte';
    import VideoView from './VideoView.svelte';
    import StreamStats from './StreamStats.svelte';
    import CameraPip from './CameraPip.svelte';
    import { recordings, startRecording, stopRecording } from '../../services/streamlog.js';
    import { autohide } from './autohide.js';
    import './call-ui.css';
    import { t } from '../../i18n/index.js';

    export let stream; // { streamId, broadcaster, mediaType, width, height, fps, qualityId }
    export let focused = false;
    export let compact = false;
    /** the broadcaster's camera, shown over their screen where the viewer puts it */
    export let camera = null;

    let box;
    let stats = false; // the live stats over the picture

    $: isOwn = stream.broadcaster === $callState.self;
    $: watching = isOwn || $callState.watching.includes(stream.streamId);
    $: media = callService.streamFor(stream.streamId, $callState, $videoStreams);
    $: name = $peerName(stream.broadcaster);
    $: label = qualityLabel(stream);
    $: speaking = !!$callState.speaking[stream.broadcaster];
    $: hasSound = !isOwn && !!callService.soundSlotOf(stream.streamId, $callState);
    $: soundOn = ($callState.streamVolume[stream.streamId] ?? 1) > 0;
    $: ownSound = isOwn && stream.streamId === $callState.screenStreamId && $callState.screenAudio;
    // our screen through the app's own capture has no preview in the webview
    $: ownNative = isOwn && stream.streamId === $callState.screenStreamId && $callState.screenNative;
    $: pipOn = !!camera && !compact && stream.mediaType !== 'camera' && watching;
    $: rec = $recordings[stream.streamId];
    $: recTime = rec ? `${Math.floor(rec.seconds / 60)}:${String(rec.seconds % 60).padStart(2, '0')}` : '';

    function toggleRecording() {
        if (rec) stopRecording(stream.streamId);
        else startRecording(stream.streamId, isOwn);
    }

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

<div bind:this={box} class="vframe speak-ring group" class:speaking class:idle-cursor={focused} use:autohide data-main-frame={focused ? '' : undefined}>
    {#if watching && media}
        <VideoView stream={media} />
    {:else if ownNative}
        <div class="ph" title={$t('call.nativeHint')}>
            <Icon name="monitor" size={focused ? 28 : 20} />
            <span class="ph-t">{$callState.screenSource?.kind === 'monitor' ? $t('call.yourWholeScreen') : $t('call.yourScreenNative', { title: $callState.screenSource?.title || '' })}</span>
            {#if focused}<span class="ph-s">{$t('call.nativeHint')}</span>{/if}
        </div>
    {:else if watching}
        <div class="ph">
            <span class="w-5 h-5 rounded-full border-2 border-white/25 border-t-white animate-spin"></span>
            <span class="ph-s">{$t('call.connectingStream')}</span>
        </div>
    {:else}
        <div class="ph">
            <button class="watch" on:click={() => callService.watchStream(stream.streamId, !compact)}>
                <Icon name="eye" size={16} /> {$t('call.watch')}
            </button>
        </div>
    {/if}

    <!-- tags that say what this is: always on, small -->
    <div class="absolute left-2 top-2 flex items-center gap-1">
        <span class="vtag live">{$t('call.live')}</span>
        {#if label}<span class="vtag">{label}</span>{/if}
        {#if rec}<span class="vtag rec-tag" title={$t('call.log.recording')}>{$t('call.log.rec')} {recTime}</span>{/if}
        {#if hasSound || ownSound}<span class="vtag" title={$t('call.streamHasSound')}><Icon name={hasSound && !soundOn ? 'volumeOff' : 'volume'} size={12} /></span>{/if}
    </div>

    <div class="ov absolute left-2 bottom-2 max-w-[70%] vtag name" style="height:24px;font-size:var(--fs-xs)">
        <span class="truncate">{isOwn ? (stream.mediaType === 'camera' ? $t('call.yourCamera') : $t('call.yourScreen')) : name}</span>
    </div>

    {#if pipOn}<CameraPip media={camera} mirror={isOwn} />{/if}

    {#if watching && stats}<StreamStats streamId={stream.streamId} {media} own={isOwn} />{/if}

    {#if watching}
        <div class="ov absolute right-2 top-2 flex items-center gap-1">
            <button class="vbtn" class:rec title={rec ? $t('call.log.stop') : $t('call.log.start')} aria-label={rec ? $t('call.log.stop') : $t('call.log.start')} aria-pressed={!!rec} on:click={toggleRecording}>
                <Icon name="record" size={16} />
            </button>
            {#if pipOn && $callState.pip.hidden}
                <button class="vbtn" title={$t('call.pip.show')} aria-label={$t('call.pip.show')} on:click={() => callService.setPip({ hidden: false })}>
                    <Icon name="video" size={16} />
                </button>
            {/if}
            <button class="vbtn" class:on={stats} title={$t('call.st.title')} aria-label={$t('call.st.title')} aria-pressed={stats} on:click={() => (stats = !stats)}>
                <Icon name="signal" size={16} />
            </button>
            {#if hasSound}
                <button class="vbtn" title={soundOn ? $t('call.streamSoundOff') : $t('call.streamSoundOn')} aria-label={soundOn ? $t('call.streamSoundOff') : $t('call.streamSoundOn')}
                    on:click={() => callService.setStreamVolume(stream.streamId, soundOn ? 0 : 1)}>
                    <Icon name={soundOn ? 'volume' : 'volumeOff'} size={16} />
                </button>
            {/if}
            {#if !isOwn}
                <button class="vbtn" title={$t('call.switchRelayTitle')} aria-label={$t('call.switchRelay')} on:click={() => callService.switchRelay(stream.streamId)}>
                    <Icon name="relay" size={16} />
                </button>
                <button class="vbtn" title={$t('call.stopWatching')} aria-label={$t('call.stopWatching')} on:click={() => callService.unwatchStream(stream.streamId)}>
                    <Icon name="eyeOff" size={16} />
                </button>
            {/if}
            <button class="vbtn" title={focused ? $t('call.backToGrid') : $t('call.focus')} aria-label={focused ? $t('call.backToGrid') : $t('call.focus')} on:click={toggleFocus}>
                <Icon name={focused ? 'collapse' : 'expand'} size={16} />
            </button>
            <button class="vbtn" title="{$t('call.fullscreen')} (F)" aria-label={$t('call.fullscreen')} on:click={fullscreen}>
                <Icon name="fullscreen" size={16} />
            </button>
        </div>
    {/if}
</div>

<style>
    .ph {
        position: absolute; inset: 0; display: flex; flex-direction: column; align-items: center; justify-content: center;
        gap: 8px; padding: 0 16px; text-align: center; background: var(--tile); color: var(--muted); font-size: var(--fs-sm);
    }
    .ph-t { font-weight: var(--fw-semibold); color: var(--text); max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .ph-s { font-size: var(--fs-xs); color: var(--faint); max-width: 34ch; }
    .watch {
        height: var(--ctl-md); padding: 0 14px; border: 0; border-radius: var(--r-md); display: inline-flex; align-items: center; gap: 8px;
        background: var(--accent); color: var(--accent-ink); font-size: var(--fs-sm); font-weight: var(--fw-semibold);
    }
    .watch:hover:not(:disabled) { background: var(--accent-hover); border: none; }
    .rec-tag::before { content: ''; width: 6px; height: 6px; border-radius: 50%; background: var(--live); animation: blink 1.4s ease-in-out infinite; }
    @keyframes blink { 50% { opacity: 0.25; } }
</style>
