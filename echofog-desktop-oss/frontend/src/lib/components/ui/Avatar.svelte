<script>
    import { colorFor, initials } from '../../stores/peers.store.js';

    export let id = '';
    export let name = '';
    export let src = '';
    export let size = 40;
    /** speaking draws the green voice ring around the avatar */
    export let speaking = false;
    /** '', 'online', 'away', 'busy' or 'offline': a status dot at the bottom right */
    export let status = '';
    export let className = '';

    $: fontSize = Math.max(11, Math.round(size * 0.38));
    $: dot = Math.max(8, Math.round(size * 0.28));
</script>

<div
    class="relative shrink-0 rounded-full select-none {className}"
    style="width:{size}px;height:{size}px"
>
    {#if src}
        <img {src} alt={name} class="w-full h-full rounded-full object-cover" />
    {:else}
        <!-- flat tint of the person's colour: no gradients -->
        <div
            class="w-full h-full rounded-full flex items-center justify-center font-semibold text-white"
            style="background:{colorFor(id || name)};font-size:{fontSize}px"
            aria-hidden="true"
        >
            {initials(name)}
        </div>
    {/if}
    <span
        class="ring pointer-events-none absolute rounded-full"
        class:on={speaking}
        style="--ring:var(--speak-ring)"
    ></span>
    {#if status}
        <span class="st {status}" style="width:{dot}px;height:{dot}px" data-status={status}></span>
    {/if}
</div>

<style>
    .ring {
        inset: -3px;
        border: 2px solid var(--ok);
        box-shadow: 0 0 0 3px var(--ring);
        opacity: 0;
        transition: opacity var(--dur-fast) var(--ease);
    }
    .ring.on { opacity: 1; }
    .st {
        position: absolute; right: -1px; bottom: -1px; border-radius: 50%;
        border: 2px solid var(--surface-1); background: var(--faint);
    }
    .st.online { background: var(--ok); }
    .st.away { background: var(--warn); }
    .st.busy { background: var(--danger); }
</style>
