<script>
    /**
     * A drop-down menu. The trigger is the default-less `trigger` slot, which gets `toggle` and `open`:
     *   <Menu items={[...]} on:select={(e) => e.detail}>
     *     <IconButton slot="trigger" let:toggle let:open icon="more" label="More" on:click={toggle} />
     *   </Menu>
     * items: [{ id, label, icon?, hint?, danger?, disabled? }] or { separator: true }.
     * Keys: arrows move, Home/End jump, Enter or Space pick, Escape closes and returns focus.
     */
    import { createEventDispatcher, tick } from 'svelte';
    import Icon from './Icon.svelte';

    export let items = [];
    export let align = 'start'; // start | end
    export let label = '';
    export let width = 220;

    const dispatch = createEventDispatcher();
    let open = false;
    let host;
    let menu;
    let style = '';
    let returnTo;

    function portal(node) {
        document.body.appendChild(node);
        return { destroy: () => node.remove() };
    }

    async function toggle() {
        if (open) return close();
        returnTo = document.activeElement;
        open = true;
        style = 'visibility:hidden';
        await tick();
        place();
        menu?.querySelector('[role="menuitem"]:not(:disabled)')?.focus();
    }
    function close(refocus = true) {
        open = false;
        if (refocus) returnTo?.focus?.();
    }
    function place() {
        if (!host || !menu) return;
        const a = host.getBoundingClientRect();
        const w = Math.min(width, window.innerWidth - 16);
        const h = menu.offsetHeight;
        let x = align === 'end' ? a.right - w : a.left;
        x = Math.max(8, Math.min(x, window.innerWidth - w - 8));
        let y = a.bottom + 4;
        if (y + h > window.innerHeight - 8) y = Math.max(8, a.top - h - 4);
        style = `left:${x}px;top:${y}px;width:${w}px;max-height:${window.innerHeight - 16}px`;
    }
    function pick(it) {
        if (it.disabled) return;
        close();
        dispatch('select', it.id);
    }
    function onKey(e) {
        const btns = [...menu.querySelectorAll('[role="menuitem"]:not(:disabled)')];
        const at = btns.indexOf(document.activeElement);
        if (e.key === 'Escape') { e.preventDefault(); close(); }
        else if (e.key === 'ArrowDown') { e.preventDefault(); btns[(at + 1) % btns.length]?.focus(); }
        else if (e.key === 'ArrowUp') { e.preventDefault(); btns[(at - 1 + btns.length) % btns.length]?.focus(); }
        else if (e.key === 'Home') { e.preventDefault(); btns[0]?.focus(); }
        else if (e.key === 'End') { e.preventDefault(); btns[btns.length - 1]?.focus(); }
        else if (e.key === 'Tab') { close(false); }
    }
</script>

<svelte:window on:resize={() => open && place()} />

<span class="host" bind:this={host}>
    <slot name="trigger" {toggle} {open} />
</span>

{#if open}
    <div class="layer" use:portal>
        <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
        <div class="backdrop" on:click={() => close(false)} on:contextmenu|preventDefault={() => close(false)}></div>
        <div class="menu" role="menu" aria-label={label || undefined} tabindex="-1" bind:this={menu} {style} on:keydown={onKey}>
            {#each items as it, i (it.id ?? 'sep' + i)}
                {#if it.separator}
                    <div class="sep" role="separator"></div>
                {:else}
                    <button type="button" role="menuitem" class="item" class:danger={it.danger} disabled={it.disabled} on:click={() => pick(it)}>
                        <span class="ic">{#if it.icon}<Icon name={it.icon} size={16} />{/if}</span>
                        <span class="txt">{it.label}</span>
                        {#if it.hint}<span class="hint">{it.hint}</span>{/if}
                    </button>
                {/if}
            {/each}
        </div>
    </div>
{/if}

<style>
    .host { display: inline-flex; }
    .layer { position: fixed; inset: 0; z-index: var(--z-menu); }
    .backdrop { position: absolute; inset: 0; }
    .menu {
        position: fixed; overflow-y: auto; padding: 4px; border-radius: var(--r-lg);
        background: var(--surface-1); border: 1px solid var(--border-strong); box-shadow: var(--shadow-lg);
        animation: fade var(--dur-fast) var(--ease);
    }
    .item {
        display: flex; align-items: center; gap: 8px; width: 100%; height: var(--ctl-md); padding: 0 8px;
        border: 0; background: transparent; border-radius: var(--r-md); text-align: left;
        font-size: var(--fs-md); font-weight: var(--fw-regular); color: var(--text);
    }
    .item:hover:not(:disabled), .item:focus-visible { background: var(--hover); border: 0; outline: none; box-shadow: none; }
    .item:focus-visible { background: var(--press); }
    .item:active:not(:disabled) { transform: none; }
    .item:disabled { opacity: 0.4; }
    .item.danger { color: var(--danger-fg); }
    .ic { width: 16px; display: inline-flex; color: var(--muted); }
    .danger .ic { color: inherit; }
    .txt { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }
    .hint { font-size: var(--fs-xs); color: var(--faint); font-variant-numeric: tabular-nums; }
    .sep { height: 1px; margin: 4px 6px; background: var(--border); }
</style>
