<script>
    /** Checkbox on a native input (so keyboard and screen readers just work). Use with bind:checked. */
    import { afterUpdate } from 'svelte';
    import Icon from './Icon.svelte';

    export let checked = false;
    export let indeterminate = false;
    export let label = '';
    export let hint = '';
    export let disabled = false;
    export let id = 'cb-' + Math.random().toString(36).slice(2, 8);

    let input;
    afterUpdate(() => { if (input) input.indeterminate = indeterminate; });
</script>

<div class="wrap" class:disabled>
    <span class="box" class:on={checked || indeterminate}>
        <input bind:this={input} {id} type="checkbox" bind:checked {disabled} aria-describedby={hint ? id + '-hint' : undefined} on:change />
        {#if indeterminate}<Icon name="minus" size={14} stroke={2.5} />
        {:else if checked}<Icon name="check" size={14} stroke={2.5} />{/if}
    </span>
    {#if label || hint}
        <div class="txt">
            {#if label}<label for={id}>{label}</label>{/if}
            {#if hint}<p id="{id}-hint">{hint}</p>{/if}
        </div>
    {/if}
</div>

<style>
    .wrap { display: flex; align-items: flex-start; gap: 10px; }
    .wrap.disabled { opacity: 0.5; }
    .box {
        position: relative; flex: none; width: 18px; height: 18px; margin-top: 2px; border-radius: var(--r-xs);
        display: inline-flex; align-items: center; justify-content: center;
        border: 1.5px solid var(--faint); color: var(--accent-ink); background: transparent;
        transition: background var(--dur-fast) var(--ease), border-color var(--dur-fast) var(--ease);
    }
    .box:hover { border-color: var(--muted); }
    .box.on { background: var(--accent); border-color: var(--accent); }
    .box:focus-within { outline: 2px solid var(--accent-ring); outline-offset: 2px; }
    input { position: absolute; inset: -4px; width: calc(100% + 8px); height: calc(100% + 8px); margin: 0; opacity: 0; cursor: pointer; }
    input:disabled { cursor: default; }
    .txt { min-width: 0; }
    label { display: block; margin: 0; font-size: var(--fs-md); line-height: 22px; color: var(--text); cursor: pointer; }
    p { margin: 0; font-size: var(--fs-xs); line-height: var(--lh-xs); color: var(--muted); }
</style>
