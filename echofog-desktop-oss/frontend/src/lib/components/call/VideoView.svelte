<script>
    /** Renders a MediaStream; re-binds whenever the stream object changes. */
    export let stream = null;
    export let mirror = false;
    export let fit = 'contain';
    export let className = '';

    let el;
    $: if (el && el.srcObject !== stream) {
        el.srcObject = stream;
        if (stream) el.play().catch(() => {});
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
