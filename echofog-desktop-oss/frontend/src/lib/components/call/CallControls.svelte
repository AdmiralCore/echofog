<script>
    import { callService, callState } from '../../services/call.service.js';
    import { toggleMute, toggleDeafen, toggleVideo, toggleScreenShare, endCall, resumeAudioContext } from '../../stores/call.store.js';
    import Icon from '../ui/Icon.svelte';
    import Popover from '../ui/Popover.svelte';
    import { t } from '../../i18n/index.js';

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

    async function measure() {
        measuring = true;
        measureError = '';
        try {
            measuredKbps = await window.go.main.App.MeasureUploadCapacity();
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
            await window.go.main.App.SetUploadCapacity(kbps);
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
            <Icon name={s.isMicMuted ? 'micOff' : 'mic'} size={21} />
        </button>
        {#if !compact}
            <button class="ctl-caret" class:open={menu === 'audio'} title={$t('call.micAndSpeakers')} aria-label={$t('call.micAndSpeakers')} on:click={() => toggleMenu('audio')}>
                <Icon name="chevronUp" size={13} stroke={2.4} />
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
        <Icon name={s.isDeafened ? 'headphonesOff' : 'headphones'} size={21} />
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
                <Icon name={s.isCameraOn ? 'video' : 'videoOff'} size={21} />
            </button>
            <button class="ctl-caret" class:open={menu === 'camera'} title={$t('call.chooseCamera')} aria-label={$t('call.camera')} on:click={() => toggleMenu('camera')}>
                <Icon name="chevronUp" size={13} stroke={2.4} />
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
            <Icon name="screenUp" size={21} />
        </button>
        {#if !compact}
            <button class="ctl-caret" class:open={menu === 'quality'} title="{$t('call.quality')} {qLabel ? '· ' + qLabel : ''}" aria-label={$t('call.quality')} on:click={() => toggleMenu('quality')}>
                <Icon name="chevronUp" size={13} stroke={2.4} />
            </button>
        {/if}
    </div>

    <div class="sep"></div>

    <button class="ctl ctl-leave" title={$t('call.leave')} aria-label={$t('call.leave')} on:click={endCall}>
        <Icon name="hangup" size={21} />
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
    .bar {
        display: flex;
        align-items: center;
        justify-content: center;
        gap: 8px;
        padding: 7px;
        border-radius: 20px;
        background: rgba(18, 19, 24, 0.78);
        backdrop-filter: blur(16px) saturate(1.3);
        border: 1px solid rgba(255, 255, 255, 0.07);
        box-shadow: 0 12px 32px rgba(0, 0, 0, 0.45);
    }
    .bar.compact {
        padding: 0;
        background: none;
        border: none;
        box-shadow: none;
        backdrop-filter: none;
    }
    .ctl {
        width: 50px;
        height: 46px;
        padding: 0;
        border: none;
        border-radius: 14px;
        display: flex;
        align-items: center;
        justify-content: center;
        background: rgba(255, 255, 255, 0.07);
        color: #e6e7ea;
        transition: background 0.15s, color 0.15s, transform 0.1s;
    }
    .ctl:hover:not(:disabled) { background: rgba(255, 255, 255, 0.13); }
    .ctl:active { transform: scale(0.96); }
    .ctl-off { background: rgba(240, 80, 80, 0.16); color: #ff7a7a; }
    .ctl-off:hover:not(:disabled) { background: rgba(240, 80, 80, 0.26); }
    .ctl-on { background: #f2f3f5; color: #15161a; }
    .ctl-on:hover:not(:disabled) { background: #ffffff; }
    .ctl-leave { width: 62px; background: #e5484d; color: #fff; }
    .ctl-leave:hover:not(:disabled) { background: #d13a3f; }
    .sep { width: 1px; height: 28px; background: rgba(255, 255, 255, 0.1); margin: 0 2px; }
    .split { position: relative; display: flex; }
    .ctl-caret {
        position: absolute;
        right: -5px;
        top: -5px;
        width: 20px;
        height: 20px;
        padding: 0;
        border-radius: 999px;
        background: #1c1d23;
        color: #b8bcc6;
        border: 1px solid rgba(255, 255, 255, 0.14);
        display: flex;
        align-items: center;
        justify-content: center;
        transition: color 0.15s, background 0.15s, transform 0.15s;
    }
    .ctl-caret:hover:not(:disabled),
    .ctl-caret.open { color: #fff; background: var(--accent); border-color: var(--accent); }
    .ctl-caret.open { transform: rotate(180deg); }

    :global(.pop .pop-title) {
        padding: 8px 10px 4px;
        font-size: 11px;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.06em;
        color: var(--faint);
    }
    :global(.pop .pop-row) {
        width: 100%;
        display: flex;
        align-items: center;
        gap: 10px;
        padding: 8px 10px;
        border: none;
        border-radius: 9px;
        background: transparent;
        color: var(--muted);
        text-align: left;
    }
    :global(.pop .pop-row:hover:not(:disabled)) { background: var(--hover); color: var(--text); }
    :global(.pop .pop-row.sel) { color: var(--text); background: color-mix(in srgb, var(--accent) 14%, transparent); }
    :global(.pop .label) { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    :global(.pop .strong) { font-weight: 600; }
    :global(.pop .tick) { color: var(--accent); }
    :global(.pop .hint) { font-size: 11px; color: var(--warn); }
    :global(.pop .rate) { font-size: 11px; color: var(--faint); font-variant-numeric: tabular-nums; }
    :global(.pop .pop-empty) { padding: 8px 10px; color: var(--faint); }
    :global(.pop .upload) {
        margin-top: 6px;
        padding: 10px 10px 6px;
        border-top: 1px solid var(--line);
        display: flex;
        flex-direction: column;
        gap: 8px;
        font-size: 12px;
        color: var(--muted);
    }
    :global(.pop .upload input) { height: 32px; padding: 4px 10px; font-size: 12px; }
    :global(.pop .upload button) { height: 32px; padding: 0 12px; }
    :global(.pop .upload .link) { height: auto; padding: 0; background: none; border: none; color: var(--accent); font-weight: 600; }
    :global(.pop .note-text) { line-height: 1.4; color: var(--faint); }
</style>
