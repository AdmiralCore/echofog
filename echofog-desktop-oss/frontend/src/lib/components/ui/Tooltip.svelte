<script>
    /**
     * A short hint on hover or keyboard focus. Wrap one control: <Tooltip text="Mute"><button/></Tooltip>.
     * The hint is drawn in <body> so nothing clips it. An empty `text` turns it off.
     */
    import { onDestroy } from 'svelte';

    export let text = '';
    export let placement = 'top'; // top | bottom | left | right
    export let delay = 350;

    let host;
    let tip;
    let shown = false;
    let style = '';
    let timer;
    const id = 'tt-' + Math.random().toString(36).slice(2, 8);

    function portal(node) {
        document.body.appendChild(node);
        return { destroy: () => node.remove() };
    }

    function place() {
        if (!host || !tip) return;
        const a = host.getBoundingClientRect();
        const w = tip.offsetWidth;
        const h = tip.offsetHeight;
        const g = 8;
        let x, y;
        if (placement === 'bottom') { x = a.left + a.width / 2 - w / 2; y = a.bottom + g; }
        else if (placement === 'left') { x = a.left - w - g; y = a.top + a.height / 2 - h / 2; }
        else if (placement === 'right') { x = a.right + g; y = a.top + a.height / 2 - h / 2; }
        else { x = a.left + a.width / 2 - w / 2; y = a.top - h - g; }
        x = Math.max(6, Math.min(x, window.innerWidth - w - 6));
        y = Math.max(6, Math.min(y, window.innerHeight - h - 6));
        style = `left:${x}px;top:${y}px`;
    }

    function show() {
        if (!text) return;
        clearTimeout(timer);
        timer = setTimeout(() => {
            shown = true;
            style = 'visibility:hidden';
            requestAnimationFrame(place);
        }, delay);
    }
    function hide() {
        clearTimeout(timer);
        shown = false;
    }
    onDestroy(() => clearTimeout(timer));
</script>

<svelte:window on:keydown={(e) => e.key === 'Escape' && hide()} />

<span
    class="host"
    bind:this={host}
    on:mouseenter={show}
    on:mouseleave={hide}
    on:focusin={show}
    on:focusout={hide}
    on:mousedown={hide}
    aria-describedby={shown ? id : undefined}
>
    <slot />
</span>

{#if shown && text}
    <div {id} class="tip" role="tooltip" bind:this={tip} use:portal {style}>{text}</div>
{/if}

<style>
    .host { display: inline-flex; }
    .tip {
        position: fixed; z-index: var(--z-tooltip); pointer-events: none; max-width: 260px;
        padding: 4px 8px; border-radius: var(--r-md); font-size: var(--fs-xs); line-height: var(--lh-xs);
        font-weight: var(--fw-medium); background: var(--text); color: var(--bg);
        box-shadow: var(--shadow-md); animation: fade var(--dur-fast) var(--ease);
    }
</style>
