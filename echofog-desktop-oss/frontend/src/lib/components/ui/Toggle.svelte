<script>
    /** On/off switch (role="switch"). Use with bind:checked. The label sits to the right; `hint` under it. */
    import { createEventDispatcher } from 'svelte';

    export let checked = false;
    export let label = '';
    export let hint = '';
    export let disabled = false;
    export let id = 'tg-' + Math.random().toString(36).slice(2, 8);

    const dispatch = createEventDispatcher();
    function flip() {
        if (disabled) return;
        checked = !checked;
        dispatch('change', checked);
    }
</script>

<div class="wrap" class:disabled>
    <button type="button" role="switch" {id} aria-checked={checked} aria-describedby={hint ? id + '-hint' : undefined} {disabled} class="sw" class:on={checked} on:click={flip} aria-label={label || undefined}>
        <span class="knob"></span>
    </button>
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
    .sw {
        position: relative; flex: none; width: 34px; height: 20px; padding: 0; margin-top: 1px;
        border-radius: var(--r-pill); border: 1px solid var(--border-strong); background: var(--surface-3);
        transition: background var(--dur) var(--ease), border-color var(--dur) var(--ease);
    }
    .sw:hover:not(:disabled) { background: var(--surface-3); border-color: var(--faint); }
    .sw:active:not(:disabled) { transform: none; }
    .sw:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 2px; box-shadow: none; }
    .sw.on, .sw.on:hover:not(:disabled) { background: var(--accent); border-color: transparent; }
    .knob {
        position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%;
        background: var(--muted); transition: transform var(--dur) var(--ease), background var(--dur) var(--ease);
    }
    .on .knob { transform: translateX(14px); background: var(--accent-ink); }
    .txt { min-width: 0; }
    label { display: block; margin: 0; font-size: var(--fs-md); line-height: 22px; font-weight: var(--fw-regular); color: var(--text); cursor: pointer; }
    p { margin: 0; font-size: var(--fs-xs); line-height: var(--lh-xs); color: var(--muted); }
</style>
