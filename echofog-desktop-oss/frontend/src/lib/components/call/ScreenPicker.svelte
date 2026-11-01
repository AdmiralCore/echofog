<script>
    /**
     * What to share with the app's own capture: the monitors, then the windows (front ones first).
     * The webview's own picker stays one click away, for a screen with its sound.
     */
    import { onDestroy } from 'svelte';
    import { callService } from '../../services/call.service.js';
    import Modal from '../ui/Modal.svelte';
    import Icon from '../ui/Icon.svelte';
    import { t } from '../../i18n/index.js';

    export let sources = [];

    $: screens = sources.filter((s) => s.kind === 'monitor');
    $: windows = sources.filter((s) => s.kind === 'window');

    // windows open and close while the list is up
    const timer = setInterval(() => callService.refreshSources(), 3000);
    onDestroy(() => clearInterval(timer));

    const pick = (s) => callService.pickedSource(s);
    const close = () => callService.pickedSource(null);
</script>

<Modal width={560} label={$t('call.pick.title')} on:close={close}>
    <div class="head">
        <span class="eyebrow grow">{$t('call.pick.title')}</span>
        <button class="icon" title={$t('call.pick.refresh')} aria-label={$t('call.pick.refresh')} on:click={() => callService.refreshSources()}><Icon name="refresh" size={16} /></button>
        <button class="icon" aria-label={$t('common.close')} on:click={close}><Icon name="close" size={17} /></button>
    </div>

    <div class="body">
        <p class="sect">{$t('call.pick.screens')}</p>
        <div class="screens">
            {#each screens as s, i (s.id)}
                <button class="screen" on:click={() => pick(s)}>
                    <Icon name="monitor" size={28} />
                    <span class="name">{$t('call.pick.screen', { n: i + 1 })}{#if s.primary}<span class="tag">{$t('call.pick.primary')}</span>{/if}</span>
                    <span class="meta">{s.width}×{s.height}{s.refreshHz ? ` · ${s.refreshHz} Hz` : ''}</span>
                </button>
            {/each}
        </div>

        <p class="sect">{$t('call.pick.windows')}</p>
        <div class="windows">
            {#each windows as w (w.id)}
                <button class="win" class:min={w.minimized} on:click={() => pick(w)} title={w.minimized ? $t('call.pick.minimized') : w.title}>
                    <Icon name="screen" size={16} />
                    <span class="title">{w.title}</span>
                    <span class="meta">{w.app ? w.app.replace(/\.exe$/i, '') : ''}{#if !w.minimized && w.width}{' · '}{w.width}×{w.height}{/if}{#if w.minimized}{' · '}{$t('call.pick.minimized')}{/if}</span>
                </button>
            {:else}
                <p class="empty">{$t('call.pick.none')}</p>
            {/each}
        </div>
    </div>

    <div class="foot">
        <p class="note">{$t('call.pick.noSound')}</p>
        <button class="link" on:click={() => pick({ webview: true })}>{$t('call.pick.useBrowser')}</button>
    </div>
</Modal>

<style>
    .head { display: flex; align-items: center; gap: 6px; padding: 14px 14px 6px 18px; }
    .grow { flex: 1; }
    .body { padding: 0 14px 6px; overflow: auto; }
    .sect { margin: 10px 4px 6px; font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.06em; color: var(--faint); }
    .screens { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 8px; }
    .screen {
        display: flex; flex-direction: column; align-items: flex-start; gap: 6px; padding: 12px;
        border-radius: 12px; border: 1px solid var(--line-soft); background: var(--raised); color: var(--text); text-align: left;
    }
    .screen:hover { border-color: var(--accent); background: var(--hover); }
    .name { font-weight: 600; display: flex; align-items: center; gap: 6px; }
    .tag { font-size: 10.5px; font-weight: 600; padding: 1px 6px; border-radius: 99px; background: var(--accent-soft); color: var(--accent); }
    .meta { font-size: 12px; color: var(--faint); font-variant-numeric: tabular-nums; }
    .windows { display: flex; flex-direction: column; gap: 2px; max-height: 300px; overflow: auto; }
    .win {
        display: grid; grid-template-columns: auto 1fr; grid-template-rows: auto auto; column-gap: 10px; align-items: center;
        padding: 8px 10px; border-radius: 9px; border: none; background: transparent; color: var(--muted); text-align: left;
    }
    .win :global(svg) { grid-row: span 2; opacity: 0.6; }
    .win:hover { background: var(--hover); color: var(--text); }
    .win.min { opacity: 0.6; }
    .title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--text); }
    .empty { padding: 8px 10px; color: var(--faint); }
    .foot { display: flex; align-items: center; gap: 12px; padding: 10px 18px 16px; border-top: 1px solid var(--line-soft); margin-top: 8px; }
    .note { flex: 1; font-size: 12px; color: var(--faint); margin: 0; }
    .link { border: none; background: none; color: var(--accent); font-weight: 600; font-size: 12.5px; padding: 4px 0; white-space: nowrap; }
</style>
