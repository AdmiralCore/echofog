<script>
    /**
     * Button. variant: primary (one per view), secondary, ghost, danger (outlined), danger-solid.
     * size: sm 28px, md 34px, lg 40px (the density setting scales them).
     */
    import Icon from './Icon.svelte';

    export let variant = 'secondary';
    export let size = 'md';
    export let icon = '';
    export let iconRight = '';
    export let loading = false;
    export let disabled = false;
    export let block = false;
    export let type = 'button';
    export let pressed = undefined; // toggle buttons: true/false
    export let className = '';

    $: iconSize = size === 'lg' ? 18 : 16;
</script>

<button
    {type}
    class="ui-btn {variant} {size} {className}"
    class:block
    class:loading
    disabled={disabled || loading}
    aria-busy={loading || undefined}
    aria-pressed={pressed}
    on:click
    on:focus
    on:blur
    {...$$restProps}
>
    {#if loading}
        <Icon name="spinner" size={iconSize} />
    {:else if icon}
        <Icon name={icon} size={iconSize} />
    {/if}
    {#if $$slots.default}<span class="lbl"><slot /></span>{/if}
    {#if iconRight && !loading}<Icon name={iconRight} size={iconSize} />{/if}
</button>

<style>
    .ui-btn {
        display: inline-flex; align-items: center; justify-content: center; gap: 6px;
        height: var(--ctl-md); padding: 0 var(--sp-3);
        border-radius: var(--r-md); border: 1px solid transparent;
        font-size: var(--fs-md); line-height: 1; font-weight: var(--fw-medium);
        background: var(--surface-2); color: var(--text); white-space: nowrap; cursor: pointer;
        transition: background var(--dur) var(--ease), border-color var(--dur) var(--ease), color var(--dur) var(--ease);
    }
    .ui-btn.sm { height: var(--ctl-sm); padding: 0 10px; font-size: var(--fs-sm); gap: 5px; }
    .ui-btn.lg { height: var(--ctl-lg); padding: 0 var(--sp-4); }
    .ui-btn.block { display: flex; width: 100%; }
    .ui-btn:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 2px; box-shadow: none; }
    .ui-btn:active:not(:disabled) { transform: none; }
    .ui-btn:disabled { opacity: 0.45; cursor: default; }

    .secondary { border-color: var(--border-strong); }
    .secondary:hover:not(:disabled) { background: var(--surface-3); border-color: var(--border-strong); }
    .primary { background: var(--accent); color: var(--accent-ink); }
    .primary:hover:not(:disabled) { background: var(--accent-hover); border-color: transparent; }
    .ghost { background: transparent; color: var(--muted); }
    .ghost:hover:not(:disabled) { background: var(--hover); color: var(--text); border-color: transparent; }
    .danger { background: transparent; color: var(--danger-fg); border-color: var(--danger-soft); }
    .danger:hover:not(:disabled) { background: var(--danger-soft); border-color: transparent; }
    .danger-solid { background: var(--danger); color: var(--danger-ink); }
    .danger-solid:hover:not(:disabled) { background: var(--danger); filter: brightness(1.08); border-color: transparent; }
    .loading { cursor: progress; }
</style>
