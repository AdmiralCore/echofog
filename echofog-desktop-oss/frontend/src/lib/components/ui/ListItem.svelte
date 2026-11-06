<script>
    /**
     * A row for lists of chats, members, settings. Slots: `lead` (avatar or icon), default (the
     * title, when `title` is not given), `meta` (time, count), `trail` (actions). With `interactive`
     * the row is a button; `selected` marks the open one.
     */
    export let title = '';
    export let subtitle = '';
    export let interactive = true;
    export let selected = false;
    export let disabled = false;
    export let unread = false; // title in bold, subtitle in full colour
</script>

<svelte:element
    this={interactive ? 'button' : 'div'}
    type={interactive ? 'button' : undefined}
    class="li"
    class:selected
    class:unread
    class:int={interactive}
    disabled={interactive ? disabled : undefined}
    aria-current={selected ? 'true' : undefined}
    on:click
    {...$$restProps}
>
    {#if $$slots.lead}<span class="lead"><slot name="lead" /></span>{/if}
    <span class="body">
        <span class="title">{#if title}{title}{:else}<slot />{/if}</span>
        {#if subtitle}<span class="sub">{subtitle}</span>{/if}
    </span>
    {#if $$slots.meta}<span class="meta"><slot name="meta" /></span>{/if}
    {#if $$slots.trail}<span class="trail"><slot name="trail" /></span>{/if}
</svelte:element>

<style>
    .li {
        display: flex; align-items: center; gap: 10px; width: 100%; min-height: var(--row-h); padding: 4px 8px;
        border-radius: var(--r-lg); border: 0; background: transparent; text-align: left; color: var(--text);
        font-size: var(--fs-md); font-weight: var(--fw-regular);
    }
    .int { cursor: pointer; transition: background var(--dur-fast) var(--ease); }
    .int:hover:not(:disabled) { background: var(--hover); border: 0; }
    .int:active:not(:disabled) { transform: none; }
    .int:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: -2px; box-shadow: none; }
    .int:disabled { opacity: 0.45; cursor: default; }
    .selected, .selected:hover:not(:disabled) { background: var(--surface-3); }
    .lead { display: inline-flex; flex: none; color: var(--muted); }
    .body { display: flex; flex-direction: column; flex: 1; min-width: 0; line-height: var(--lh-md); }
    .title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: var(--fw-medium); }
    .unread .title { font-weight: var(--fw-semibold); }
    .sub { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: var(--fs-sm); line-height: var(--lh-sm); color: var(--muted); }
    .unread .sub { color: var(--text); }
    .meta { flex: none; font-size: var(--fs-xs); color: var(--faint); font-variant-numeric: tabular-nums; display: inline-flex; flex-direction: column; align-items: flex-end; gap: 2px; }
    .trail { flex: none; display: inline-flex; align-items: center; gap: 2px; }
</style>
