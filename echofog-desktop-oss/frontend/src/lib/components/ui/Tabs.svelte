<script>
    /**
     * Tabs. items: [{ id, label, icon?, count?, disabled? }]; bind:value is the chosen id.
     * variant "line" (underline, for pages) or "segment" (a pill group, for compact switches).
     * Arrow keys move between tabs, Home and End jump to the first and last.
     */
    import { createEventDispatcher } from 'svelte';
    import Icon from './Icon.svelte';

    export let items = [];
    export let value = '';
    export let variant = 'line';
    export let label = '';

    const dispatch = createEventDispatcher();
    const uid = Math.random().toString(36).slice(2, 7);
    let list;

    function pick(id) {
        if (id === value) return;
        value = id;
        dispatch('change', id);
    }

    function onKey(e) {
        const enabled = items.filter((i) => !i.disabled);
        const at = enabled.findIndex((i) => i.id === value);
        let next = -1;
        if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = (at + 1) % enabled.length;
        else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') next = (at - 1 + enabled.length) % enabled.length;
        else if (e.key === 'Home') next = 0;
        else if (e.key === 'End') next = enabled.length - 1;
        if (next < 0) return;
        e.preventDefault();
        pick(enabled[next].id);
        list.querySelector(`[data-id="${CSS.escape(String(enabled[next].id))}"]`)?.focus();
    }
</script>

<div class="tabs {variant}" role="tablist" aria-label={label || undefined} bind:this={list} on:keydown={onKey}>
    {#each items as it (it.id)}
        <button
            type="button"
            role="tab"
            id="tab-{uid}-{it.id}"
            data-id={it.id}
            aria-selected={value === it.id}
            aria-controls={'panel-' + uid + '-' + it.id}
            tabindex={value === it.id ? 0 : -1}
            disabled={it.disabled}
            class:on={value === it.id}
            on:click={() => pick(it.id)}
        >
            {#if it.icon}<Icon name={it.icon} size={16} />{/if}
            <span>{it.label}</span>
            {#if it.count !== undefined && it.count !== null}<span class="cnt">{it.count}</span>{/if}
        </button>
    {/each}
</div>

<style>
    .tabs { display: flex; gap: 2px; }
    button {
        display: inline-flex; align-items: center; gap: 6px; height: var(--ctl-md); padding: 0 var(--sp-3);
        border: 0; background: transparent; color: var(--muted); border-radius: var(--r-md);
        font-size: var(--fs-md); font-weight: var(--fw-medium); position: relative;
        transition: color var(--dur) var(--ease), background var(--dur) var(--ease);
    }
    button:hover:not(:disabled):not(.on) { color: var(--text); background: var(--hover); border: 0; }
    button:active:not(:disabled) { transform: none; }
    button:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: -2px; box-shadow: none; }
    button:disabled { opacity: 0.4; }
    button.on { color: var(--text); }
    .cnt { font-size: var(--fs-2xs); padding: 1px 6px; border-radius: var(--r-pill); background: var(--badge-bg); color: var(--badge-text); font-variant-numeric: tabular-nums; }

    .line { border-bottom: 1px solid var(--border); gap: 4px; }
    .line button { border-radius: var(--r-md) var(--r-md) 0 0; height: calc(var(--ctl-md) + 4px); }
    .line button.on::after { content: ""; position: absolute; left: 8px; right: 8px; bottom: -1px; height: 2px; border-radius: 2px; background: var(--accent); }

    .segment { display: inline-flex; padding: 2px; background: var(--surface-2); border-radius: var(--r-lg); }
    .segment button { height: calc(var(--ctl-md) - 6px); border-radius: var(--r-md); font-size: var(--fs-sm); }
    .segment button.on { background: var(--seg-on); box-shadow: var(--shadow-sm); }
</style>
