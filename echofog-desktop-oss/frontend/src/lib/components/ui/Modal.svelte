<script context="module">
  // the windows open now, the newest last: Escape closes only the top one
  const open = [];
</script>

<script>
  // A window over the app for a panel of its own (a group's settings, a confirmation that asks for
  // more than yes or no). Escape or a click outside closes it.
  import { createEventDispatcher, onDestroy } from 'svelte';
  import { fade, scale } from 'svelte/transition';

  export let label = '';
  export let width = 480; // px at most; narrower windows get all of their width
  export let wide = false; // the whole window on small screens, edge to edge
  export let on = true; // false: just what it holds, in place

  const dispatch = createEventDispatcher();
  const me = {};
  $: if (on && !open.includes(me)) open.push(me);
  $: if (!on && open.includes(me)) open.splice(open.indexOf(me), 1);
  onDestroy(() => open.includes(me) && open.splice(open.indexOf(me), 1));
  // closed after the event that asked for it is done: what holds the window is taken down outside
  // of that event's handling
  const close = () => setTimeout(() => dispatch('close'));
  function onKey(e) {
    if (e.key === 'Escape' && open[open.length - 1] === me) { e.preventDefault(); close(); }
  }
</script>

<svelte:window on:keydown={(e) => on && onKey(e)} />

{#if on}
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" class:wide in:fade={{ duration: 120 }} on:click|self={close}>
    <div class="win" role="dialog" aria-modal="true" aria-label={label} style="--w:{width}px" in:scale={{ start: 0.97, duration: 160 }}>
      <slot />
    </div>
  </div>
{:else}
  <slot />
{/if}

<style>
  .backdrop {
    position: fixed; inset: 0; z-index: var(--z-modal); display: grid; place-items: center; padding: 16px;
    background: var(--scrim);
  }
  .win {
    width: min(var(--w), 100%); max-height: calc(100vh - 32px); overflow: auto;
    border-radius: var(--r-xl); background: var(--surface-1);
    border: 1px solid var(--border-strong); box-shadow: var(--shadow-xl);
    display: flex; flex-direction: column;
  }
  @media (max-width: 640px) {
    .backdrop.wide { padding: 0; }
    .backdrop.wide .win { width: 100%; height: 100%; max-height: none; border-radius: 0; border: none; }
  }
</style>
