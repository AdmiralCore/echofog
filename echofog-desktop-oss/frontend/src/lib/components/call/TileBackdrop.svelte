<script>
    import { onDestroy, onMount } from 'svelte';
    import { EFFECTS } from './backdrops/effects.js';
    import { animate, reducedMotion } from './backdrops/loop.js';

    /** backdrop name ("matrix", "waves", …); empty or unknown is the plain look */
    export let effect = '';
    /** the person's colour, '#rrggbb' */
    export let color = '#65aadd';
    /** speaking livens the backdrop up */
    export let speaking = false;

    let box;
    let canvas;
    let w = 0;
    let h = 0;
    let visible = false;
    let draw = null;
    let energy = 0;
    let stop = null;

    $: fx = EFFECTS[effect];
    $: build(fx, color, w, h, canvas);
    // drawn only while on screen; with reduced motion the first frame stays
    $: run(draw, visible && !$reducedMotion);

    function build(fx, color, w, h, canvas) {
        draw = null;
        if (!fx || !canvas || !w || !h) return;
        const k = Math.min(fx.scale * Math.min(window.devicePixelRatio || 1, 1.5), Math.sqrt(fx.max / (w * h)));
        canvas.width = Math.max(1, Math.round(w * k));
        canvas.height = Math.max(1, Math.round(h * k));
        const ctx = canvas.getContext('2d', { alpha: false });
        if (!ctx) return;
        draw = fx.make(ctx, canvas.width, canvas.height, color);
        draw(performance.now() / 1000, 0, 0);
    }

    function run(draw, on) {
        stop?.();
        stop = null;
        if (!draw || !on) return;
        stop = animate((t, dt) => {
            energy += ((speaking ? 1 : 0) - energy) * Math.min(1, dt * 4);
            draw(t, dt, energy);
        });
    }

    let sizes;
    let seen;
    onMount(() => {
        sizes = new ResizeObserver(([e]) => {
            const r = e.contentRect;
            // a few pixels of layout jitter do not restart the effect
            if (Math.abs(r.width - w) > 2 || Math.abs(r.height - h) > 2) {
                w = Math.round(r.width);
                h = Math.round(r.height);
            }
        });
        sizes.observe(box);
        seen = new IntersectionObserver(([e]) => (visible = e.isIntersecting));
        seen.observe(box);
    });

    onDestroy(() => {
        stop?.();
        sizes?.disconnect();
        seen?.disconnect();
    });
</script>

<div bind:this={box} class="absolute inset-0 pointer-events-none" aria-hidden="true">
    {#if fx}
        <canvas bind:this={canvas} class="block w-full h-full"></canvas>
    {:else}
        <div class="w-full h-full" style="background:radial-gradient(circle at 50% 40%, rgba(255,255,255,.05), transparent 70%)"></div>
    {/if}
</div>
