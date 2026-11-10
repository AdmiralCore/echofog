<script>
    /**
     * A menu that opens above (or below) an anchor element. It lives in <body>, above everything,
     * so no parent clips it or covers it, and its height follows the room there is: it scrolls.
     */
    import { onMount, onDestroy, createEventDispatcher, tick } from 'svelte';

    export let anchor; // element the menu belongs to
    export let width = 300;
    export let label = '';

    const dispatch = createEventDispatcher();
    let el;
    let style = 'visibility:hidden';

    function portal(node) {
        document.body.appendChild(node);
        return { destroy: () => node.remove() };
    }

    async function place() {
        await tick();
        if (!anchor || !el) return;
        const a = anchor.getBoundingClientRect();
        const vw = window.innerWidth;
        const vh = window.innerHeight;
        const gap = 10;
        const w = Math.min(width, vw - 16);
        const left = Math.max(8, Math.min(a.left + a.width / 2 - w / 2, vw - w - 8));
        const above = a.top - gap - 8;
        const below = vh - a.bottom - gap - 8;
        if (above >= 220 || above >= below) {
            style = `left:${left}px;bottom:${vh - a.top + gap}px;width:${w}px;max-height:${Math.max(160, above)}px`;
        } else {
            style = `left:${left}px;top:${a.bottom + gap}px;width:${w}px;max-height:${Math.max(160, below)}px`;
        }
    }

    const close = () => dispatch('close');
    const onKey = (e) => e.key === 'Escape' && close();

    onMount(() => {
        place();
        window.addEventListener('resize', place);
    });
    onDestroy(() => window.removeEventListener('resize', place));
</script>

<svelte:window on:keydown={onKey} />

<div use:portal class="pop-layer">
    <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
    <div class="pop-backdrop" on:click={close}></div>
    <div class="pop" bind:this={el} {style} role="dialog" aria-label={label}>
        <slot />
    </div>
</div>

<style>
    .pop-layer {
        position: fixed;
        inset: 0;
        z-index: var(--z-menu);
        pointer-events: none;
    }
    .pop-backdrop {
        position: absolute;
        inset: 0;
        pointer-events: auto;
    }
    .pop {
        position: fixed;
        pointer-events: auto;
        overflow-y: auto;
        overscroll-behavior: contain;
        padding: 6px;
        border-radius: var(--r-xl);
        background: var(--surface-1);
        border: 1px solid var(--border-strong);
        box-shadow: var(--shadow-lg);
        color: var(--text);
        font-size: var(--fs-md);
        animation: pop-in var(--dur) var(--ease);
    }
    @keyframes pop-in {
        from { opacity: 0; transform: translateY(4px); }
        to { opacity: 1; transform: none; }
    }
</style>
