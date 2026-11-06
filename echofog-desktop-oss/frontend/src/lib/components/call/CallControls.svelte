<script>
    import { onDestroy } from 'svelte';
    import { callService, callState } from '../../services/call.service.js';
    import { toggleMute, toggleDeafen, toggleVideo, toggleScreenShare, endCall, resumeAudioContext } from '../../stores/call.store.js';
    import Icon from '../ui/Icon.svelte';
    import Popover from '../ui/Popover.svelte';
    import Pills from '../ui/Pills.svelte';
    import { t } from '../../i18n/index.js';
    import { App } from '../../api.js';

    export let compact = false;

    let menu = ''; // '' | 'audio' | 'camera' | 'quality'
    let micAnchor, camAnchor, shareAnchor;
    let measuring = false;
    let measuredKbps = 0;
    let measureError = '';
    let manualMbps = '';

    $: s = $callState;
    $: currentQ = s.isScreenSharing ? s.streamQualityId : s.selectedQualityId;
    $: qLabel = s.qualities.find((q) => q.id === currentQ)?.label;

    function toggleMenu(name) {
        menu = menu === name ? '' : name;
        if (menu === 'audio' || menu === 'camera') callService.refreshDevices();
        if (menu === 'quality' && !measuredKbps && !measuring) measure();
    }

    // while the quality menu is open on a shared screen: how it is really encoded, every 2 s
    let enc = null;
    let encTimer = null;
    $: watchEncoder(menu === 'quality' && s.isScreenSharing);
    function watchEncoder(on) {
        clearInterval(encTimer);
        encTimer = null;
        enc = null;
        if (!on) return;
        const read = async () => { try { enc = await callService.screenEncoderInfo(); } catch (_) { enc = null; } };
        read();
        encTimer = setInterval(read, 2000);
    }
    onDestroy(() => clearInterval(encTimer));

    let mode = $callState.screenMode;
    $: modeItems = [
        { value: 'motion', label: $t('call.mode.motion'), hint: $t('call.mode.motionHint') },
        { value: 'detail', label: $t('call.mode.detail'), hint: $t('call.mode.detailHint') },
    ];
    $: if (mode !== s.screenMode) callService.setScreenMode(mode);

    let capture = $callState.screenCapture;
    $: captureItems = [
        { value: 'native', label: $t('call.capture.native'), hint: $t('call.capture.nativeHint') },
        { value: 'webview', label: $t('call.capture.webview'), hint: $t('call.capture.webviewHint') },
    ];
    $: if (capture !== s.screenCapture) callService.setScreenCapture(capture);

    async function measure() {
        measuring = true;
        measureError = '';
        try {
            measuredKbps = await App.MeasureUploadCapacity();
            await callService.loadQualities();
        } catch (e) {
            measureError = String(e?.message || e);
        } finally {
            measuring = false;
        }
    }

    async function applyManual() {
        const kbps = Math.round(Number(manualMbps) * 1000);
        if (!(kbps > 0)) return;
        try {
            await App.SetUploadCapacity(kbps);
            measuredKbps = kbps;
            await callService.loadQualities();
        } catch (e) {
            measureError = String(e?.message || e);
        }
    }

    async function pickQuality(q) {
        menu = '';
        if (s.isScreenSharing) await callService.setStreamQuality(q.id);
        else {
            callState.update((c) => ({ ...c, selectedQualityId: q.id }));
            resumeAudioContext();
            await callService.startScreenShare(q.id);
        }
    }

    const mbps = (kbps) => (kbps / 1000).toFixed(kbps % 1000 ? 1 : 0);
</script>

<div class="bar {compact ? 'compact' : ''}">
    <div class="split" bind:this={micAnchor}>
        <button
            class="ctl {s.isMicMuted ? 'ctl-off' : ''}"
            title={s.hasMic ? (s.isMicMuted ? $t('call.unmute') : $t('call.mute')) : $t('call.noMic')}
            aria-label={s.isMicMuted ? $t('call.unmute') : $t('call.mute')}
            aria-pressed={s.isMicMuted}
            on:click={toggleMute}
        >
            <Icon name={s.isMicMuted ? 'micOff' : 'mic'} size={18} />
        </button>
        {#if !compact}
            <button class="ctl-caret" class:open={menu === 'audio'} title={$t('call.micAndSpeakers')} aria-label={$t('call.micAndSpeakers')} on:click={() => toggleMenu('audio')}>
                <Icon name="chevronUp" size={12} stroke={2} />
            </button>
        {/if}
    </div>

    <button
        class="ctl {s.isDeafened ? 'ctl-off' : ''}"
        title={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')}
        aria-label={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')}
        aria-pressed={s.isDeafened}
        on:click={toggleDeafen}
    >
        <Icon name={s.isDeafened ? 'headphonesOff' : 'headphones'} size={18} />
    </button>

    {#if !compact}
        <div class="split" bind:this={camAnchor}>
            <button
                class="ctl {s.isCameraOn ? 'ctl-on' : ''}"
                title={s.isCameraOn ? $t('call.cameraOff') : $t('call.cameraOn')}
                aria-label={s.isCameraOn ? $t('call.cameraOff') : $t('call.cameraOn')}
                aria-pressed={s.isCameraOn}
                on:click={toggleVideo}
            >
                <Icon name={s.isCameraOn ? 'video' : 'videoOff'} size={18} />
            </button>
            <button class="ctl-caret" class:open={menu === 'camera'} title={$t('call.chooseCamera')} aria-label={$t('call.camera')} on:click={() => toggleMenu('camera')}>
                <Icon name="chevronUp" size={12} stroke={2} />
            </button>
        </div>
    {/if}

    <div class="split" bind:this={shareAnchor}>
        <button
            class="ctl {s.isScreenSharing ? 'ctl-on' : ''}"
            title={s.isScreenSharing ? $t('call.stopSharing') : $t('call.shareYourScreen')}
            aria-label={s.isScreenSharing ? $t('call.stopSharing') : $t('call.shareYourScreen')}
            aria-pressed={s.isScreenSharing}
            on:click={toggleScreenShare}
        >
            <Icon name="screenUp" size={18} />
        </button>
        {#if !compact}
            <button class="ctl-caret" class:open={menu === 'quality'} title="{$t('call.quality')} {qLabel ? '· ' + qLabel : ''}" aria-label={$t('call.quality')} on:click={() => toggleMenu('quality')}>
                <Icon name="chevronUp" size={12} stroke={2} />
            </button>
        {/if}
    </div>

    <div class="sep"></div>

    <button class="ctl ctl-leave" title={$t('call.leave')} aria-label={$t('call.leave')} on:click={endCall}>
        <Icon name="hangup" size={18} />
    </button>
</div>

{#if menu === 'audio'}
    <Popover anchor={micAnchor} label={$t('call.micAndSpeakers')} on:close={() => (menu = '')}>
        <p class="pop-title">{$t('call.microphone')}</p>
        {#each s.devices.mics as d (d.id)}
            <button class="pop-row" class:sel={d.id === s.micDeviceId || (!s.micDeviceId && d.id === 'default')} on:click={() => callService.setMicDevice(d.id)}>
                <Icon name="mic" size={15} className="opacity-60" />
                <span class="label">{d.label}</span>
                {#if d.id === s.micDeviceId || (!s.micDeviceId && d.id === 'default')}<Icon name="check" size={15} className="tick" />{/if}
            </button>
        {:else}
            <p class="pop-empty">{$t('call.noMics')}</p>
        {/each}
        <p class="pop-title">{$t('call.speakers')}</p>
        <button class="pop-row" class:sel={!s.outDeviceId} on:click={() => callService.setOutputDevice('')}>
            <Icon name="volume" size={15} className="opacity-60" />
            <span class="label">{$t('call.systemDefault')}</span>
            {#if !s.outDeviceId}<Icon name="check" size={15} className="tick" />{/if}
        </button>
        {#each s.devices.outs.filter((d) => d.id && d.id !== 'default') as d (d.id)}
            <button class="pop-row" class:sel={d.id === s.outDeviceId} on:click={() => callService.setOutputDevice(d.id)}>
                <Icon name="volume" size={15} className="opacity-60" />
                <span class="label">{d.label}</span>
                {#if d.id === s.outDeviceId}<Icon name="check" size={15} className="tick" />{/if}
            </button>
        {/each}
    </Popover>
{:else if menu === 'camera'}
    <Popover anchor={camAnchor} label={$t('call.camera')} on:close={() => (menu = '')}>
        <p class="pop-title">{$t('call.camera')}</p>
        {#each s.devices.cams as d (d.id)}
            <button class="pop-row" class:sel={d.id === s.camDeviceId} on:click={() => { callService.setCameraDevice(d.id); menu = ''; }}>
                <Icon name="video" size={15} className="opacity-60" />
                <span class="label">{d.label}</span>
                {#if d.id === s.camDeviceId}<Icon name="check" size={15} className="tick" />{/if}
            </button>
        {:else}
            <p class="pop-empty">{$t('call.noCams')}</p>
        {/each}
    </Popover>
{:else if menu === 'quality'}
    <Popover anchor={shareAnchor} width={320} label={$t('call.quality')} on:close={() => (menu = '')}>
        {#if s.nativeScreen.available}
            <p class="pop-title">{$t('call.capture')}</p>
            <div class="mode"><Pills items={captureItems} bind:value={capture} label={$t('call.capture')} /></div>
            <p class="pop-hint">{capture === 'native' ? $t('call.capture.nativeHint') : $t('call.capture.webviewHint')}</p>
        {:else if s.nativeScreen.reason}
            <p class="pop-hint">{$t('call.capture.unavailable', { reason: s.nativeScreen.reason })}</p>
        {/if}
        {#if !s.nativeScreen.available || capture === 'webview'}
            <p class="pop-title">{$t('call.mode')}</p>
            <div class="mode"><Pills items={modeItems} bind:value={mode} label={$t('call.mode')} /></div>
            <p class="pop-hint">{mode === 'detail' ? $t('call.mode.detailHint') : $t('call.mode.motionHint')}</p>
        {/if}
        {#if enc}
            <p class="enc" class:warn={enc.hardware === false || enc.limit === 'cpu'}>
                {enc.width}×{enc.height} · {enc.fps} {$t('call.fps')} · {mbps(enc.kbps)} {$t('unit.mbps')}
                {#if enc.encoder}{' · '}{enc.hardware === false ? $t('call.enc.cpu') : enc.hardware ? $t('call.enc.gpu') : enc.encoder}{/if}
                {#if enc.limit}{' · '}{$t('call.enc.limit.' + enc.limit)}{/if}
            </p>
        {/if}
        <p class="pop-title">{$t('call.quality')}</p>
        {#each s.qualities as q (q.id)}
            <button class="pop-row" class:sel={q.id === currentQ} role="option" aria-selected={q.id === currentQ} on:click={() => pickQuality(q)}>
                <span class="label strong">{q.label}</span>
                {#if !q.fits}<span class="hint" title={$t('call.needsUploadHint')}>{$t('call.needsUpload')}</span>{/if}
                <span class="rate">{mbps(q.bitrateKbps)} {$t('unit.mbps')}</span>
                {#if q.id === currentQ}<Icon name="check" size={15} className="tick" />{/if}
            </button>
        {:else}
            <p class="pop-empty">{$t('call.loadingPresets')}</p>
        {/each}
        <div class="upload">
            <div class="row">
                <span class="grow">{#if measuring}{$t('call.measuring')}{:else if measuredKbps}{$t('call.yourUpload', { v: mbps(measuredKbps) + ' ' + $t('unit.mbps') })}{:else}{$t('call.uploadUnknown')}{/if}</span>
                <button class="link" disabled={measuring} on:click={measure}>{$t('call.measure')}</button>
            </div>
            {#if measureError}<p class="error">{measureError}</p>{/if}
            <form class="row" on:submit|preventDefault={applyManual}>
                <input type="number" min="1" placeholder={$t('call.uploadPlaceholder')} aria-label={$t('call.uploadAria')} bind:value={manualMbps} />
                <button type="submit">{$t('call.set')}</button>
            </form>
            <p class="note-text">{$t('call.qualityNote')}</p>
        </div>
    </Popover>
{/if}

<style>
    /* one dense bar under the video: 36 px controls, the arrow of a split button is part of it */
    .bar {
        display: flex; align-items: center; justify-content: center; gap: 6px; padding: 5px;
        border-radius: var(--r-xl); background: var(--surface-2); border: 1px solid var(--border);
    }
    .bar.compact { padding: 0; background: none; border: none; }
    .ctl {
        width: 40px; height: var(--ctl-lg); padding: 0; border: none; border-radius: var(--r-md);
        display: flex; align-items: center; justify-content: center;
        background: var(--surface-3); color: var(--text);
        transition: background var(--dur-fast) var(--ease), color var(--dur-fast) var(--ease);
    }
    .ctl:hover:not(:disabled) { background: var(--press); border: none; }
    .ctl:active { transform: none; }
    .ctl:focus-visible, .ctl-caret:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 1px; }
    .ctl-off { background: var(--danger-soft); color: var(--danger-fg); }
    .ctl-off:hover:not(:disabled) { background: var(--danger-soft); filter: brightness(1.12); }
    .ctl-on { background: var(--accent); color: var(--accent-ink); }
    .ctl-on:hover:not(:disabled) { background: var(--accent-hover); }
    .ctl-leave { width: 48px; background: var(--danger); color: var(--danger-ink); }
    .ctl-leave:hover:not(:disabled) { background: var(--danger); filter: brightness(1.1); }
    .sep { width: 1px; height: 22px; background: var(--border-strong); margin: 0 2px; }
    .split { position: relative; display: flex; }
    .split .ctl { border-top-right-radius: 0; border-bottom-right-radius: 0; }
    .bar.compact .split .ctl { border-radius: var(--r-md); }
    .ctl-caret {
        width: 18px; height: var(--ctl-lg); padding: 0; border: none; border-left: 1px solid var(--border);
        border-radius: 0 var(--r-md) var(--r-md) 0; background: var(--surface-3); color: var(--muted);
        display: flex; align-items: center; justify-content: center;
        transition: color var(--dur-fast) var(--ease), background var(--dur-fast) var(--ease);
    }
    .ctl-caret:hover:not(:disabled), .ctl-caret.open { color: var(--text); background: var(--press); border-left-color: var(--border); }
    .ctl-caret.open :global(svg) { transform: rotate(180deg); }
    .split:has(.ctl-on) .ctl-caret { background: var(--accent); color: var(--accent-ink); border-left-color: rgba(0, 0, 0, 0.18); }
    .split:has(.ctl-off) .ctl-caret { background: var(--danger-soft); color: var(--danger-fg); }

    :global(.pop .pop-title) {
        padding: 8px 10px 4px; font-size: var(--fs-2xs); font-weight: var(--fw-semibold); text-transform: uppercase;
        letter-spacing: 0.06em; color: var(--faint);
    }
    :global(.pop .pop-row) {
        width: 100%; display: flex; align-items: center; gap: 10px; padding: 7px 10px; border: none;
        border-radius: var(--r-md); background: transparent; color: var(--muted); text-align: left; font-size: var(--fs-sm);
    }
    :global(.pop .pop-row:hover:not(:disabled)) { background: var(--hover); color: var(--text); }
    :global(.pop .pop-row.sel) { color: var(--text); background: var(--accent-soft); }
    :global(.pop .label) { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    :global(.pop .strong) { font-weight: var(--fw-semibold); }
    :global(.pop .tick) { color: var(--accent-fg); }
    :global(.pop .hint) { font-size: var(--fs-2xs); color: var(--warn-fg); }
    :global(.pop .rate) { font-size: var(--fs-2xs); color: var(--faint); font-variant-numeric: tabular-nums; }
    :global(.pop .pop-empty) { padding: 8px 10px; color: var(--faint); font-size: var(--fs-sm); }
    :global(.pop .upload) {
        margin-top: 6px; padding: 10px 10px 6px; border-top: 1px solid var(--border-strong);
        display: flex; flex-direction: column; gap: 8px; font-size: var(--fs-xs); color: var(--muted);
    }
    :global(.pop .upload input) { height: var(--ctl-sm); padding: 4px 10px; font-size: var(--fs-xs); }
    :global(.pop .upload button) { height: var(--ctl-sm); padding: 0 12px; }
    :global(.pop .upload .link) { height: auto; padding: 0; background: none; border: none; color: var(--accent-fg); font-weight: var(--fw-semibold); }
    :global(.pop .note-text) { line-height: 1.4; color: var(--faint); }
    .mode { padding: 2px 10px 4px; }
    .pop-hint { margin: 0 10px 6px; font-size: var(--fs-xs); line-height: 1.35; color: var(--faint); }
    .enc { margin: 2px 10px 8px; padding: 6px 9px; border-radius: var(--r-md); font-size: var(--fs-xs); font-variant-numeric: tabular-nums; background: var(--surface-3); color: var(--muted); }
    .enc.warn { color: var(--warn-fg); }
</style>
