<script>
    /**
     * A scrolling region with thin scrollbars and soft edges that show there is more above or below.
     * Give it a height (or put it in a flex column); it scrolls inside. Focusable, so the keyboard can scroll it.
     */
    import { onMount } from 'svelte';

    export let label = '';
    export let maxHeight = '';
    export let horizontal = false;
    export let el = undefined; // the scrolling element

    let top = false;
    let bottom = false;

    function measure() {
        if (!el) return;
        top = el.scrollTop > 2;
        bottom = el.scrollTop + el.clientHeight < el.scrollHeight - 2;
    }
    onMount(() => {
        measure();
        const ro = new ResizeObserver(measure);
        ro.observe(el);
        return () => ro.disconnect();
    });
</script>

<div class="area" class:top class:bottom>
    <!-- svelte-ignore a11y-no-noninteractive-tabindex (a scrolling region must be reachable by keyboard) -->
    <div
        class="scroll"
        class:h={horizontal}
        bind:this={el}
        on:scroll={measure}
        tabindex="0"
        role="region"
        aria-label={label || undefined}
        style={maxHeight ? `max-height:${maxHeight}` : ''}
    >
        <slot />
    </div>
</div>

<style>
    .area { position: relative; min-height: 0; display: flex; flex-direction: column; }
    .area::before, .area::after {
        content: ""; position: absolute; left: 0; right: 0; height: 16px; z-index: 1; pointer-events: none;
        opacity: 0; transition: opacity var(--dur) var(--ease);
    }
    .area::before { top: 0; background: linear-gradient(var(--bg), transparent); }
    .area::after { bottom: 0; background: linear-gradient(transparent, var(--bg)); }
    .area.top::before, .area.bottom::after { opacity: 0.9; }
    .scroll { flex: 1; min-height: 0; overflow-y: auto; overflow-x: hidden; overscroll-behavior: contain; border-radius: inherit; }
    .scroll.h { overflow-x: auto; }
    .scroll:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: -2px; }
</style>
