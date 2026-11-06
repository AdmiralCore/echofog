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
    import IconButton from '../ui/IconButton.svelte';
    import Checkbox from '../ui/Checkbox.svelte';
    import Button from '../ui/Button.svelte';
    import Badge from '../ui/Badge.svelte';
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

<Modal width={800} wide label={$t('call.pick.title')} on:close={close}>
    <header>
        <h2 class="grow">{$t('call.pick.title')}</h2>
        <IconButton icon="refresh" label={$t('call.pick.refresh')} on:click={() => callService.refreshSources()} />
        <IconButton icon="close" label={$t('common.close')} on:click={close} />
    </header>

    <div class="body">
        <p class="eyebrow">{$t('call.pick.screens')}</p>
        <div class="grid screens">
            {#each screens as s, i (s.id)}
                <button class="card" on:click={() => pick(s)}>
                    <span class="shot">
                        {#if previews[s.id]}<img src={previews[s.id]} alt="" />{:else}<Icon name="monitor" size={30} />{/if}
                    </span>
                    <span class="name">{$t('call.pick.screen', { n: i + 1 })}{#if s.primary}<Badge variant="accent" size="sm">{$t('call.pick.primary')}</Badge>{/if}</span>
                    <span class="meta">{s.width}×{s.height}{s.refreshHz ? ` · ${s.refreshHz} Hz` : ''}</span>
                </button>
            {/each}
        </div>

        <p class="eyebrow">{$t('call.pick.windows')}</p>
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
                <p class="empty small muted">{$t('call.pick.none')}</p>
            {/each}
        </div>
    </div>

    <footer>
        <div class="grow"><Checkbox bind:checked={sound} label={$t('call.pick.sound')} hint={$t('call.pick.soundHint')} /></div>
        <Button variant="ghost" on:click={() => pick({ webview: true })}>{$t('call.pick.useBrowser')}</Button>
    </footer>
</Modal>

<style>
    header { display: flex; align-items: center; gap: var(--sp-1); padding: var(--sp-4) var(--sp-4) var(--sp-2) var(--sp-5); }
    header h2 { font-size: var(--fs-lg); line-height: var(--lh-lg); }
    .body { padding: 0 var(--sp-4) var(--sp-3) var(--sp-5); overflow: auto; min-height: 0; }
    .eyebrow { margin: var(--sp-3) 0 var(--sp-2); }
    .grid { display: grid; gap: var(--sp-3); }
    .screens { grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); }
    .windows { grid-template-columns: repeat(auto-fill, minmax(165px, 1fr)); }
    .card {
        display: flex; flex-direction: column; align-items: stretch; gap: 4px; padding: var(--sp-2); min-width: 0;
        border-radius: var(--r-lg); border: 1px solid var(--border); background: var(--surface-2); color: var(--text); text-align: left;
        transition: border-color var(--dur), background var(--dur);
    }
    .card:hover:not(:disabled) { border-color: var(--accent); background: var(--hover); }
    .card:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 1px; }
    .card.min { opacity: 0.55; }
    .shot {
        aspect-ratio: 16 / 9; border-radius: var(--r-md); overflow: hidden; background: var(--tile);
        display: flex; align-items: center; justify-content: center; color: var(--faint); margin-bottom: 4px;
    }
    .shot img { width: 100%; height: 100%; object-fit: contain; display: block; }
    .name { font-weight: var(--fw-semibold); font-size: var(--fs-sm); display: flex; align-items: center; gap: 6px; min-width: 0; }
    .title { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .meta { font-size: var(--fs-xs); color: var(--faint); font-variant-numeric: tabular-nums; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .empty { padding: var(--sp-2); }
    footer { display: flex; align-items: center; gap: var(--sp-3); padding: var(--sp-3) var(--sp-5); border-top: 1px solid var(--border); background: var(--surface-2); }
</style>
