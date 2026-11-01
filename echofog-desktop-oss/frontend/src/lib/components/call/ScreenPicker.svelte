<script>
    /**
     * What to share with the app's own capture: the monitors, then the windows (front ones first),
     * each with a live thumbnail so people see which is which. The webview's own picker stays one
     * click away.
     */
    import { onDestroy } from 'svelte';
    import { callService, callState } from '../../services/call.service.js';
    import Modal from '../ui/Modal.svelte';
    import Icon from '../ui/Icon.svelte';
    import { t } from '../../i18n/index.js';

    export let sources = [];

    $: screens = sources.filter((s) => s.kind === 'monitor');
    $: windows = sources.filter((s) => s.kind === 'window');
    // the core keeps a dozen captures at most; minimized windows send nothing to show
    $: live = sources.filter((s) => !s.minimized).map((s) => s.id);

    let previews = {};
    let busy = false;
    async function poll() {
        if (busy) return;
        busy = true;
        previews = await callService.screenPreviews(live);
        busy = false;
    }
    const pollTimer = setInterval(poll, 300);
    poll();
    // windows open and close while the list is up
    const listTimer = setInterval(() => callService.refreshSources(), 3000);
    onDestroy(() => {
        clearInterval(pollTimer);
        clearInterval(listTimer);
        callService.screenPreviews([]);
    });

    let sound = $callState.screenSound;
    $: if (sound !== $callState.screenSound) callService.setScreenSound(sound);

    const pick = (s) => callService.pickedSource(s);
    const close = () => callService.pickedSource(null);
    const app = (w) => (w.app ? w.app.replace(/\.exe$/i, '') : '');
</script>

<Modal width={760} wide label={$t('call.pick.title')} on:close={close}>
    <div class="head">
        <span class="eyebrow grow">{$t('call.pick.title')}</span>
        <button class="icon" title={$t('call.pick.refresh')} aria-label={$t('call.pick.refresh')} on:click={() => callService.refreshSources()}><Icon name="refresh" size={16} /></button>
        <button class="icon" aria-label={$t('common.close')} on:click={close}><Icon name="close" size={17} /></button>
    </div>

    <div class="body">
        <p class="sect">{$t('call.pick.screens')}</p>
        <div class="grid screens">
            {#each screens as s, i (s.id)}
                <button class="card" on:click={() => pick(s)}>
                    <span class="shot">
                        {#if previews[s.id]}<img src={previews[s.id]} alt="" />{:else}<Icon name="monitor" size={30} />{/if}
                    </span>
                    <span class="name">{$t('call.pick.screen', { n: i + 1 })}{#if s.primary}<span class="tag">{$t('call.pick.primary')}</span>{/if}</span>
                    <span class="meta">{s.width}×{s.height}{s.refreshHz ? ` · ${s.refreshHz} Hz` : ''}</span>
                </button>
            {/each}
        </div>

        <p class="sect">{$t('call.pick.windows')}</p>
        <div class="grid windows">
            {#each windows as w (w.id)}
                <button class="card" class:min={w.minimized} on:click={() => pick(w)} title={w.minimized ? $t('call.pick.minimized') : w.title}>
                    <span class="shot">
                        {#if previews[w.id]}<img src={previews[w.id]} alt="" />{:else}<Icon name="screen" size={22} />{/if}
                    </span>
                    <span class="name title">{w.title}</span>
                    <span class="meta">{app(w)}{#if w.minimized}{' · '}{$t('call.pick.minimizedShort')}{:else if w.width}{' · '}{w.width}×{w.height}{/if}</span>
                </button>
            {:else}
                <p class="empty">{$t('call.pick.none')}</p>
            {/each}
        </div>
    </div>

    <div class="foot">
        <label class="sound" title={$t('call.pick.soundHint')}>
            <input type="checkbox" bind:checked={sound} />
            <span>{$t('call.pick.sound')}<span class="hint">{$t('call.pick.soundHint')}</span></span>
        </label>
        <button class="link" on:click={() => pick({ webview: true })}>{$t('call.pick.useBrowser')}</button>
    </div>
</Modal>

<style>
    .head { display: flex; align-items: center; gap: 6px; padding: 14px 14px 6px 18px; }
    .grow { flex: 1; }
    .body { padding: 0 14px 6px; overflow: auto; }
    .sect { margin: 10px 4px 6px; font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.06em; color: var(--faint); }
    .grid { display: grid; gap: 10px; }
    .screens { grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); }
    .windows { grid-template-columns: repeat(auto-fill, minmax(165px, 1fr)); }
    .card {
        display: flex; flex-direction: column; align-items: stretch; gap: 4px; padding: 8px; min-width: 0;
        border-radius: 12px; border: 1px solid var(--line-soft); background: var(--raised); color: var(--text); text-align: left;
        transition: border-color var(--dur, 0.15s), background var(--dur, 0.15s);
    }
    .card:hover { border-color: var(--accent); background: var(--hover); }
    .card.min { opacity: 0.55; }
    .shot {
        aspect-ratio: 16 / 9; border-radius: 8px; overflow: hidden; background: #0d0e12;
        display: flex; align-items: center; justify-content: center; color: var(--faint); margin-bottom: 4px;
    }
    .shot img { width: 100%; height: 100%; object-fit: contain; display: block; }
    .name { font-weight: 600; font-size: 13px; display: flex; align-items: center; gap: 6px; min-width: 0; }
    .title { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .tag { font-size: 10.5px; font-weight: 600; padding: 1px 6px; border-radius: 99px; background: var(--accent-soft); color: var(--accent); }
    .meta { font-size: 11.5px; color: var(--faint); font-variant-numeric: tabular-nums; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .empty { padding: 8px 10px; color: var(--faint); }
    .foot { display: flex; align-items: center; gap: 12px; padding: 12px 18px 16px; border-top: 1px solid var(--line-soft); margin-top: 8px; }
    .sound { flex: 1; display: flex; align-items: flex-start; gap: 8px; font-size: 13px; color: var(--text); cursor: pointer; }
    .sound input { margin-top: 2px; accent-color: var(--accent); }
    .hint { display: block; font-size: 11.5px; color: var(--faint); }
    .link { border: none; background: none; color: var(--accent); font-weight: 600; font-size: 12.5px; padding: 4px 0; white-space: nowrap; }
</style>
