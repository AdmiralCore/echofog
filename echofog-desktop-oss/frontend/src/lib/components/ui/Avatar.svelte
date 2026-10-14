<script>
    import { colorFor, initials } from '../../stores/peers.store.js';

    export let id = '';
    export let name = '';
    export let src = '';
    export let size = 40;
    /** speaking draws the green voice ring around the avatar */
    export let speaking = false;
    export let className = '';

    $: fontSize = Math.max(11, Math.round(size * 0.38));
</script>

<div
    class="relative shrink-0 rounded-full select-none {className}"
    style="width:{size}px;height:{size}px"
>
    {#if src}
        <img {src} alt={name} class="w-full h-full rounded-full object-cover" />
    {:else}
        <div
            class="w-full h-full rounded-full flex items-center justify-center font-bold text-white"
            style="background:linear-gradient(135deg, {colorFor(id || name)}, {colorFor((id || name) + '~')});font-size:{fontSize}px"
        >
            {initials(name)}
        </div>
    {/if}
    <span
        class="pointer-events-none absolute -inset-[3px] rounded-full border-[3px] transition-opacity duration-150 {speaking ? 'opacity-100' : 'opacity-0'}"
        style="border-color:var(--color-success)"
    ></span>
</div>
