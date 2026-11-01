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
    .picker { display: grid; grid-template-columns: repeat(4, 1fr); gap: 10px; }
    .picker.compact { grid-template-columns: repeat(3, 1fr); gap: 6px; }
    .opt {
        display: flex;
        flex-direction: column;
        gap: 5px;
        padding: 4px;
        border: 1px solid transparent;
        border-radius: 12px;
        background: transparent;
        color: var(--muted);
        transition: border-color 0.15s, background 0.15s, box-shadow 0.15s;
    }
    .opt:hover:not(:disabled) { border-color: var(--line); background: var(--raised); }
    .opt.sel, .opt.sel:hover:not(:disabled) { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); color: var(--accent); }
    .pv { position: relative; display: block; aspect-ratio: 16 / 9; border-radius: 8px; overflow: hidden; background: #1e1f22; }
    .label { font-size: 12px; font-weight: 500; text-align: center; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
    .compact .opt { padding: 3px; border-radius: 10px; gap: 3px; }
    .compact .pv { border-radius: 7px; }
    .compact .label { font-size: 11px; }
</style>
