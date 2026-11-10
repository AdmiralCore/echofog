<script>
    /** A grey placeholder while something loads. Shapes: a bar (width, height), a circle, or `lines` of text. */
    import { t } from '../../i18n/index.js';

    export let width = '100%';
    export let height = 14;
    export let circle = false;
    export let lines = 1;

    $: w = typeof width === 'number' ? width + 'px' : width;
    $: h = typeof height === 'number' ? height + 'px' : height;
</script>

<span class="sk-wrap" role="status" aria-label={$t('ui.loading')}>
    {#if circle}
        <span class="sk circle" style="width:{h};height:{h}"></span>
    {:else}
        {#each Array(lines) as _, i}
            <span class="sk" style="width:{lines > 1 && i === lines - 1 ? '60%' : w};height:{h}"></span>
        {/each}
    {/if}
</span>

<style>
    .sk-wrap { display: flex; flex-direction: column; gap: 8px; }
    .sk {
        display: block; border-radius: var(--r-sm); background: var(--surface-3);
        background-image: linear-gradient(90deg, transparent, var(--hover), transparent);
        background-size: 200% 100%; animation: sk 1.4s ease-in-out infinite;
    }
    .circle { border-radius: 50%; }
    @keyframes sk { from { background-position: 150% 0; } to { background-position: -50% 0; } }
</style>
