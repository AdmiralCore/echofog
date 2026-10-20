<script>
    /** Renders a MediaStream; re-binds whenever the stream object changes. */
    export let stream = null;
    export let mirror = false;
    export let fit = 'contain';
    export let className = '';
    import { log, errAttrs } from '../../uilog.js';

    let el;
    $: if (el && el.srcObject !== stream) {
        el.srcObject = stream;
        // AbortError only means the next stream replaced this one before it started
        if (stream) el.play().catch((e) => { if (e?.name !== 'AbortError') log.warn('call', 'video did not start playing', errAttrs(e)); });
    }
</script>

<!-- svelte-ignore a11y-media-has-caption -->
<video
    bind:this={el}
    autoplay
    playsinline
    muted
    class="w-full h-full bg-black {className}"
    style="object-fit:{fit};{mirror ? 'transform:scaleX(-1);' : ''}"
></video>
