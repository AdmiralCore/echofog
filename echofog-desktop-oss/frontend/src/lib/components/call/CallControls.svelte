<script>
    import { callService, callState } from '../../services/call.service.js';
    import { toggleMute, toggleDeafen, toggleVideo, toggleScreenShare, endCall, resumeAudioContext } from '../../stores/call.store.js';
    import Icon from '../ui/Icon.svelte';
    import Popover from '../ui/Popover.svelte';

    export let compact = false;

    let menu = ''; // '' | 'devices' | 'quality'
    let devAnchor, qAnchor; // the menus open from these buttons and float above everything
    let measuring = false;
    let measuredKbps = 0;
    let measureError = '';
    let manualMbps = '';

    $: s = $callState;
    $: currentQ = s.isScreenSharing ? s.streamQualityId : s.selectedQualityId;
    $: qLabel = s.qualities.find((q) => q.id === currentQ)?.label;

    function toggleMenu(name) {
        menu = menu === name ? '' : name;
        if (menu === 'devices') callService.refreshDevices();
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

<div class="relative flex items-center justify-center gap-2 {compact ? '' : 'p-1.5 rounded-2xl bg-[#111214]/90 backdrop-blur border border-white/5 shadow-lg'}">
    <div class="split" bind:this={devAnchor}>
        <button
            class="ctl {s.isMicMuted ? 'ctl-off' : ''}"
            title={s.hasMic ? (s.isMicMuted ? 'Unmute' : 'Mute') : 'No microphone'}
            aria-label={s.isMicMuted ? 'Unmute' : 'Mute'}
            aria-pressed={s.isMicMuted}
            on:click={toggleMute}
        >
            <Icon name={s.isMicMuted ? 'micOff' : 'mic'} size={22} />
        </button>
        {#if !compact}
            <button class="ctl-caret" title="Audio and video devices" aria-label="Devices" on:click={() => toggleMenu('devices')}>
                <Icon name="chevronUp" size={14} stroke={2.4} />
            </button>
        {/if}
    </div>

    <button
        class="ctl {s.isDeafened ? 'ctl-off' : ''}"
        title={s.isDeafened ? 'Undeafen' : 'Deafen'}
        aria-label={s.isDeafened ? 'Undeafen' : 'Deafen'}
        aria-pressed={s.isDeafened}
        on:click={toggleDeafen}
    >
        <Icon name={s.isDeafened ? 'headphonesOff' : 'headphones'} size={22} />
    </button>

    {#if !compact}
        <button
            class="ctl {s.isCameraOn ? 'ctl-on' : ''}"
            title={s.isCameraOn ? 'Turn off camera' : 'Turn on camera'}
            aria-label={s.isCameraOn ? 'Turn off camera' : 'Turn on camera'}
            aria-pressed={s.isCameraOn}
            on:click={toggleVideo}
        >
            <Icon name={s.isCameraOn ? 'video' : 'videoOff'} size={22} />
        </button>
    {/if}

    <div class="split" bind:this={qAnchor}>
        <button
            class="ctl {s.isScreenSharing ? 'ctl-on' : ''}"
            title={s.isScreenSharing ? 'Stop sharing' : 'Share your screen'}
            aria-label={s.isScreenSharing ? 'Stop sharing' : 'Share your screen'}
            aria-pressed={s.isScreenSharing}
            on:click={toggleScreenShare}
        >
            <Icon name="screenUp" size={22} />
        </button>
        {#if !compact}
            <button class="ctl-caret" title="Stream quality {qLabel ? '· ' + qLabel : ''}" aria-label="Stream quality" on:click={() => toggleMenu('quality')}>
                <Icon name="chevronUp" size={14} stroke={2.4} />
            </button>
        {/if}
    </div>

    <div class="w-px h-8 bg-white/10 mx-1"></div>

    <button class="ctl ctl-leave" title="Leave call" aria-label="Leave call" on:click={endCall}>
        <Icon name="hangup" size={22} />
    </button>

    {#if menu === 'devices'}
        <Popover anchor={devAnchor} width={300} label="Audio and video devices" on:close={() => (menu = '')}>
            <p class="pop-title">Microphone</p>
            {#each s.devices.mics as d (d.id)}
                <button class="pop-row" on:click={() => { callService.setMicDevice(d.id); menu = ''; }}>
                    <span class="truncate flex-1 text-left">{d.label}</span>
                    {#if d.id === s.micDeviceId || (!s.micDeviceId && d.id === 'default')}<Icon name="check" size={16} className="text-[var(--color-accent)]" />{/if}
                </button>
            {:else}
                <p class="pop-empty">No microphones found</p>
            {/each}
            {#if s.devices.outs && s.devices.outs.length}
                <p class="pop-title mt-2">Speakers</p>
                <button class="pop-row" on:click={() => { callService.setOutputDevice(''); menu = ''; }}>
                    <span class="truncate flex-1 text-left">System default</span>
                    {#if !s.outDeviceId}<Icon name="check" size={16} className="text-[var(--color-accent)]" />{/if}
                </button>
                {#each s.devices.outs.filter((d) => d.id && d.id !== 'default') as d (d.id)}
                    <button class="pop-row" on:click={() => { callService.setOutputDevice(d.id); menu = ''; }}>
                        <span class="truncate flex-1 text-left">{d.label}</span>
                        {#if d.id === s.outDeviceId}<Icon name="check" size={16} className="text-[var(--color-accent)]" />{/if}
                    </button>
                {/each}
            {/if}
            <p class="pop-title mt-2">Camera</p>
            {#each s.devices.cams as d (d.id)}
                <button class="pop-row" on:click={() => { callService.setCameraDevice(d.id); menu = ''; }}>
                    <span class="truncate flex-1 text-left">{d.label}</span>
                    {#if d.id === s.camDeviceId}<Icon name="check" size={16} className="text-[var(--color-accent)]" />{/if}
                </button>
            {:else}
                <p class="pop-empty">No cameras found</p>
            {/each}
        </Popover>
    {:else if menu === 'quality'}
        <Popover anchor={qAnchor} width={300} label="Stream quality" on:close={() => (menu = '')}>
            <p class="pop-title">Stream quality</p>
            {#each s.qualities as q (q.id)}
                <button class="pop-row {q.id === currentQ ? '!text-white' : ''}" role="option" aria-selected={q.id === currentQ} disabled={!q.fits} on:click={() => pickQuality(q)}>
                    <span class="flex-1 text-left font-semibold">{q.label}</span>
                    <span class="text-xs text-[#949ba4]">{mbps(q.bitrateKbps)} Mbps</span>
                    {#if q.id === currentQ}<Icon name="check" size={16} className="text-[var(--color-accent)]" />{/if}
                </button>
            {:else}
                <p class="pop-empty">Loading presets…</p>
            {/each}
            <div class="border-t border-white/10 mt-2 pt-2 px-2 text-xs text-[#949ba4]">
                <div class="flex items-center justify-between">
                    <span>{#if measuring}Measuring upload…{:else if measuredKbps}Your upload: {mbps(measuredKbps)} Mbps{:else}Upload speed unknown{/if}</span>
                    <button class="text-[var(--color-accent)] font-semibold disabled:opacity-40" disabled={measuring} on:click={measure}>Measure</button>
                </div>
                {#if measureError}<p class="text-[#f23f43] mt-1">{measureError}</p>{/if}
                <div class="flex gap-1.5 mt-2">
                    <input class="flex-1 h-8 rounded-md bg-black/40 border border-white/10 px-2 text-white text-xs outline-none focus:border-[var(--color-accent)]" type="number" min="1" placeholder="Upload, Mbps" aria-label="Upload speed in Mbps" bind:value={manualMbps} />
                    <button class="h-8 px-3 rounded-md bg-white/10 text-white font-semibold hover:bg-white/15" on:click={applyManual}>Set</button>
                </div>
                <p class="mt-2 leading-snug">Viewers relay your stream to each other, so 4K60 needs only enough upload for two copies.</p>
            </div>
        </Popover>
    {/if}
</div>

<style>
    .ctl {
        width: 52px;
        height: 48px;
        border-radius: 14px;
        display: flex;
        align-items: center;
        justify-content: center;
        background: #2b2d31;
        color: #dbdee1;
        transition: background 0.12s, color 0.12s;
    }
    .ctl:hover {
        background: #35373c;
        color: #fff;
    }
    .ctl-off {
        background: rgba(242, 63, 67, 0.15);
        color: #f23f43;
    }
    .ctl-off:hover {
        background: rgba(242, 63, 67, 0.25);
        color: #f23f43;
    }
    .ctl-on {
        background: #fff;
        color: #111214;
    }
    .ctl-on:hover {
        background: #e3e5e8;
        color: #111214;
    }
    .ctl-leave {
        width: 64px;
        background: #da373c;
        color: #fff;
    }
    .ctl-leave:hover {
        background: #a12828;
        color: #fff;
    }
    .split {
        position: relative;
        display: flex;
    }
    .ctl-caret {
        position: absolute;
        right: -4px;
        top: -4px;
        width: 20px;
        height: 20px;
        border-radius: 9999px;
        background: #111214;
        color: #b5bac1;
        border: 1px solid rgba(255, 255, 255, 0.12);
        display: flex;
        align-items: center;
        justify-content: center;
    }
    .ctl-caret:hover {
        color: #fff;
    }
    .pop-title {
        padding: 6px 8px 4px;
        font-size: 11px;
        font-weight: 700;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: #949ba4;
    }
    .pop-row {
        width: 100%;
        display: flex;
        align-items: center;
        gap: 8px;
        padding: 7px 8px;
        border-radius: 6px;
        color: #b5bac1;
    }
    .pop-row:hover:not(:disabled) {
        background: rgba(255, 255, 255, 0.06);
        color: #fff;
    }
    .pop-row:disabled {
        opacity: 0.35;
        cursor: default;
    }
    .pop-empty {
        padding: 6px 8px;
        color: #6d6f78;
    }
</style>
