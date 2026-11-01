<script>
    /**
     * The broadcaster's camera over their screen, placed by the viewer: dragged anywhere in the
     * tile, resized from its corner, in the shape the viewer picks, or hidden. It is the camera
     * stream the call already carries, laid out here; nothing is mixed into the screen's video.
     * Where it sits is remembered as fractions of the free room, so it stays put when the tile
     * changes size.
     */
    import { callService, callState } from '../../services/call.service.js';
    import VideoView from './VideoView.svelte';
    import Icon from '../ui/Icon.svelte';
    import { t } from '../../i18n/index.js';

    export let media;
    export let mirror = false;

    // width / height of each shape
    const SHAPES = { circle: 1, rounded: 4 / 3, square: 1, hexagon: 1.155, wide: 16 / 9 };
    const MARGIN = 12;
    const MIN_W = 0.1;
    const MAX_W = 0.5;

    let W = 0;
    let H = 0;
    $: pip = $callState.pip;
    $: shape = SHAPES[pip.shape] ? pip.shape : 'circle';
    $: w = Math.max(64, Math.min(W * pip.w, W - 2 * MARGIN, (H - 2 * MARGIN) * SHAPES[shape]));
    $: h = w / SHAPES[shape];
    $: left = MARGIN + pip.x * Math.max(0, W - w - 2 * MARGIN);
    $: top = MARGIN + pip.y * Math.max(0, H - h - 2 * MARGIN);

    let drag = null; // { kind: 'move' | 'size', px, py, x, y, w }

    function start(e, kind) {
        if (e.button !== 0) return;
        e.preventDefault();
        e.stopPropagation();
        e.currentTarget.setPointerCapture(e.pointerId);
        drag = { kind, px: e.clientX, py: e.clientY, x: pip.x, y: pip.y, w: pip.w };
    }

    function move(e) {
        if (!drag) return;
        const dx = e.clientX - drag.px;
        const dy = e.clientY - drag.py;
        if (drag.kind === 'move') {
            const roomX = Math.max(1, W - w - 2 * MARGIN);
            const roomY = Math.max(1, H - h - 2 * MARGIN);
            callService.setPip({ x: clamp(drag.x + dx / roomX, 0, 1), y: clamp(drag.y + dy / roomY, 0, 1) }, false);
        } else {
            // the handle sits on the corner that faces the middle of the tile
            const sign = pip.x > 0.5 ? -1 : 1;
            callService.setPip({ w: clamp(drag.w + (sign * dx) / Math.max(1, W), MIN_W, MAX_W) }, false);
        }
    }

    function end() {
        if (!drag) return;
        drag = null;
        callService.setPip({}, true); // remembered once it is let go
    }

    const clamp = (v, a, b) => Math.min(b, Math.max(a, v));
</script>

<div class="room" bind:clientWidth={W} bind:clientHeight={H}>
    {#if W > 0 && !pip.hidden}
        <!-- svelte-ignore a11y-no-static-element-interactions -->
        <div
            class="pip shape-{shape}"
            class:dragging={drag}
            style="left:{left}px;top:{top}px;width:{w}px;height:{h}px"
            on:pointerdown={(e) => start(e, 'move')}
            on:pointermove={move}
            on:pointerup={end}
            on:pointercancel={end}
            title={$t('call.pip.drag')}
        >
            <div class="frame"><VideoView stream={media} fit="cover" {mirror} /></div>
            <div class="bar" on:pointerdown|stopPropagation>
                {#each Object.keys(SHAPES) as s (s)}
                    <button class="swatch" class:on={s === shape} title={$t('call.pip.shape.' + s)} aria-label={$t('call.pip.shape.' + s)} aria-pressed={s === shape}
                        on:click|stopPropagation={() => callService.setPip({ shape: s })}><span class="glyph glyph-{s}"></span></button>
                {/each}
                <button class="swatch" title={$t('call.pip.hide')} aria-label={$t('call.pip.hide')} on:click|stopPropagation={() => callService.setPip({ hidden: true })}>
                    <Icon name="close" size={12} stroke={2.4} />
                </button>
            </div>
            <!-- svelte-ignore a11y-no-static-element-interactions -->
            <div class="handle" class:left={pip.x > 0.5} title={$t('call.pip.resize')}
                on:pointerdown={(e) => start(e, 'size')} on:pointermove={move} on:pointerup={end} on:pointercancel={end}></div>
        </div>
    {/if}
</div>

<style>
    .room { position: absolute; inset: 0; pointer-events: none; overflow: hidden; }
    .pip {
        position: absolute; pointer-events: auto; cursor: grab; touch-action: none;
        filter: drop-shadow(0 6px 16px rgba(0, 0, 0, 0.55));
    }
    .pip.dragging { cursor: grabbing; }
    .frame { width: 100%; height: 100%; overflow: hidden; background: #000; }
    .shape-circle .frame { border-radius: 50%; }
    .shape-rounded .frame { border-radius: 18px; }
    .shape-square .frame { border-radius: 4px; }
    .shape-wide .frame { border-radius: 10px; }
    .shape-hexagon .frame { clip-path: polygon(25% 0, 75% 0, 100% 50%, 75% 100%, 25% 100%, 0 50%); }
    .bar {
        position: absolute; left: 50%; top: 6px; transform: translateX(-50%);
        display: flex; gap: 3px; padding: 3px; border-radius: 99px; background: rgba(0, 0, 0, 0.7);
        opacity: 0; transition: opacity 0.15s; cursor: default;
    }
    .pip:hover .bar, .pip:focus-within .bar { opacity: 1; }
    .swatch {
        width: 20px; height: 20px; padding: 0; border: none; border-radius: 99px; background: transparent; color: #fff;
        display: flex; align-items: center; justify-content: center;
    }
    .swatch:hover:not(:disabled), .swatch.on { background: rgba(255, 255, 255, 0.22); border: none; }
    .glyph { display: block; width: 11px; height: 11px; background: #fff; }
    .glyph-circle { border-radius: 50%; }
    .glyph-rounded { width: 13px; height: 10px; border-radius: 3px; }
    .glyph-square { border-radius: 1px; }
    .glyph-wide { width: 14px; height: 8px; border-radius: 2px; }
    .glyph-hexagon { width: 12px; clip-path: polygon(25% 0, 75% 0, 100% 50%, 75% 100%, 25% 100%, 0 50%); }
    .handle {
        position: absolute; right: -2px; bottom: -2px; width: 18px; height: 18px; cursor: nwse-resize;
        border-right: 3px solid rgba(255, 255, 255, 0.85); border-bottom: 3px solid rgba(255, 255, 255, 0.85);
        border-bottom-right-radius: 6px; opacity: 0; transition: opacity 0.15s;
    }
    .handle.left {
        right: auto; left: -2px; cursor: nesw-resize; border-right: none; border-left: 3px solid rgba(255, 255, 255, 0.85);
        border-bottom-right-radius: 0; border-bottom-left-radius: 6px;
    }
    .pip:hover .handle { opacity: 1; }
</style>
