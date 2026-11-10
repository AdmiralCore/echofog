<script>
    import { BACKDROPS, backdropSettings } from '../../stores/backdrops.js';
    import { colorFor } from '../../stores/peers.store.js';
    import TileBackdrop from './TileBackdrop.svelte';
    import { t } from '../../i18n/index.js';

    /** whose colour the previews take */
    export let peer = '';
    /** three small previews a row, for the call menu */
    export let compact = false;

    function pick(name) {
        backdropSettings.update((s) => ({ ...s, mine: name }));
    }
</script>

<div class="picker" class:compact role="radiogroup" aria-label={$t('call.bg.title')}>
    {#each BACKDROPS as name (name)}
        <button class="opt" class:sel={$backdropSettings.mine === name} role="radio" aria-checked={$backdropSettings.mine === name} on:click={() => pick(name)}>
            <span class="pv"><TileBackdrop effect={name} color={colorFor(peer)} /></span>
            <span class="label">{$t('call.bg.' + (name || 'plain'))}</span>
        </button>
    {/each}
</div>

<style>
    .picker { display: grid; grid-template-columns: repeat(4, 1fr); gap: var(--sp-3); }
    .picker.compact { grid-template-columns: repeat(3, 1fr); gap: var(--sp-2); }
    .opt {
        display: flex; flex-direction: column; gap: 5px; padding: 4px;
        border: 1px solid transparent; border-radius: var(--r-lg); background: transparent; color: var(--muted);
        transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
    }
    .opt:hover:not(:disabled) { border-color: var(--border-strong); background: var(--hover); }
    .opt:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 1px; }
    .opt.sel, .opt.sel:hover:not(:disabled) { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); color: var(--accent-fg); }
    .pv { position: relative; display: block; aspect-ratio: 16 / 9; border-radius: var(--r-md); overflow: hidden; background: var(--tile); }
    .label { font-size: var(--fs-xs); font-weight: var(--fw-medium); text-align: center; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
    .compact .opt { padding: 3px; border-radius: var(--r-md); gap: 3px; }
    .compact .pv { border-radius: var(--r-sm); }
    .compact .label { font-size: var(--fs-2xs); }
</style>
