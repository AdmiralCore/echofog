<script>
    /**
     * A surface that groups content. variant: raised (default), flat, outlined. `padding`: none | sm | md | lg.
     * Slots: header, default, footer. With `interactive` the whole card is one button.
     */
    export let variant = 'raised';
    export let padding = 'md';
    export let interactive = false;
    export let selected = false;
</script>

{#if interactive}
    <button type="button" class="ui-card {variant} p-{padding} int" class:selected on:click {...$$restProps}>
        <slot />
    </button>
{:else}
    <section class="ui-card {variant} p-{padding}" class:selected {...$$restProps}>
        {#if $$slots.header}<header><slot name="header" /></header>{/if}
        <slot />
        {#if $$slots.footer}<footer><slot name="footer" /></footer>{/if}
    </section>
{/if}

<style>
    .ui-card {
        display: block; width: 100%; text-align: left; border-radius: var(--r-xl); color: var(--text);
        background: var(--surface-2); border: 1px solid transparent;
    }
    .raised { box-shadow: var(--shadow-sm); border-color: var(--border); background: var(--surface-1); }
    .flat { background: var(--surface-2); }
    .outlined { background: transparent; border-color: var(--border-strong); }
    .p-none { padding: 0; }
    .p-sm { padding: var(--sp-3); }
    .p-md { padding: var(--sp-4); }
    .p-lg { padding: var(--sp-6); }
    header { margin-bottom: var(--sp-3); }
    footer { margin-top: var(--sp-4); display: flex; justify-content: flex-end; gap: var(--sp-2); }
    .int { cursor: pointer; font-weight: var(--fw-regular); white-space: normal; font-size: var(--fs-md); transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease); }
    .int:hover:not(:disabled) { border-color: var(--border-strong); background: var(--surface-2); }
    .int:active:not(:disabled) { transform: none; }
    .int:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 2px; box-shadow: none; }
    .selected { border-color: var(--accent); background: var(--accent-soft); }
    .selected.int:hover:not(:disabled) { background: var(--accent-soft); border-color: var(--accent); }
</style>
