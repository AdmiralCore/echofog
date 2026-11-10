<script>
    /** A thin rule. With text in the default slot it becomes a labelled divider ("Today"). */
    export let vertical = false;
    export let inset = 0; // px of space at the start (to line up with a list's text)
</script>

{#if vertical}
    <div class="v" role="separator" aria-orientation="vertical"></div>
{:else if $$slots.default}
    <div class="lbl" role="separator" style="margin-left:{inset}px"><span><slot /></span></div>
{:else}
    <hr style="margin-left:{inset}px" />
{/if}

<style>
    hr { border: 0; height: 1px; margin: var(--sp-2) 0; background: var(--border); }
    .v { width: 1px; align-self: stretch; background: var(--border); margin: 0 var(--sp-2); }
    .lbl { display: flex; align-items: center; gap: var(--sp-3); margin: var(--sp-3) 0; font-size: var(--fs-xs); font-weight: var(--fw-semibold); color: var(--faint); }
    .lbl::before, .lbl::after { content: ""; flex: 1; height: 1px; background: var(--border); }
</style>
