<script>
    /** A square button with only an icon. `label` is required: it is the accessible name and the tooltip. */
    import Icon from './Icon.svelte';
    import Tooltip from './Tooltip.svelte';

    export let icon;
    export let label;
    export let variant = 'ghost'; // ghost | subtle | accent | danger | live
    export let size = 'md'; // sm 28, md 34, lg 40
    export let pressed = undefined;
    export let disabled = false;
    export let tooltip = true;
    export let placement = 'top';
    export let type = 'button';

    $: iconSize = size === 'lg' ? 20 : size === 'sm' ? 16 : 18;
</script>

<Tooltip text={tooltip ? label : ''} {placement}>
    <button
        {type}
        class="ui-ib {variant} {size}"
        class:on={pressed}
        aria-label={label}
        aria-pressed={pressed}
        {disabled}
        on:click
        {...$$restProps}
    >
        <Icon name={icon} size={iconSize} />
    </button>
</Tooltip>

<style>
    .ui-ib {
        display: inline-flex; align-items: center; justify-content: center; padding: 0;
        width: var(--ctl-md); height: var(--ctl-md); border-radius: var(--r-md);
        border: 1px solid transparent; background: transparent; color: var(--muted); cursor: pointer;
        transition: background var(--dur) var(--ease), color var(--dur) var(--ease);
    }
    .sm { width: var(--ctl-sm); height: var(--ctl-sm); }
    .lg { width: var(--ctl-lg); height: var(--ctl-lg); }
    .ui-ib:hover:not(:disabled) { background: var(--hover); color: var(--text); border-color: transparent; }
    .ui-ib:active:not(:disabled) { transform: none; }
    .ui-ib:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 1px; box-shadow: none; }
    .ui-ib:disabled { opacity: 0.45; cursor: default; }
    .subtle { background: var(--surface-2); color: var(--text); }
    .subtle:hover:not(:disabled) { background: var(--surface-3); }
    .on { background: var(--accent-soft); color: var(--accent-fg); }
    .on:hover:not(:disabled) { background: var(--accent-soft); color: var(--accent-fg); }
    .accent { background: var(--accent); color: var(--accent-ink); }
    .accent:hover:not(:disabled) { background: var(--accent-hover); color: var(--accent-ink); }
    .danger { color: var(--danger-fg); }
    .danger:hover:not(:disabled) { background: var(--danger-soft); color: var(--danger-fg); }
    .live { background: var(--danger); color: var(--danger-ink); }
    .live:hover:not(:disabled) { background: var(--danger); color: var(--danger-ink); filter: brightness(1.08); }
</style>
