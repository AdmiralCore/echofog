<script>
  import { writable } from 'svelte/store';
  import { panelHidden } from '../../stores/panels.js';

  /** Holds a side column (the list of personal chats, a space's channels). Shown, it is the column in
   *  place. Hidden, it is a 12 px strip by the rail: the pointer on it slides the column over the
   *  content, leaving it slides it away, a click pins it back. */
  export let kind = 'personal'; // personal | space
  export let label = '';
  export let off = false; // a window with no room for it: always the column in place, filling the width

  $: hidden = off ? writable(false) : panelHidden[kind];
  let peek = false;
  let timer;
  function enter() { clearTimeout(timer); peek = true; }
  function leave() { clearTimeout(timer); timer = setTimeout(() => (peek = false), 220); }
  $: if (!$hidden) peek = false;
</script>

<!-- svelte-ignore a11y-no-static-element-interactions -->
<div class="host" class:hidden={$hidden} class:peek class:off on:mouseenter={() => $hidden && enter()} on:mouseleave={() => $hidden && leave()} on:focusin={() => $hidden && enter()}>
  {#if $hidden}
    <button class="strip" aria-label={label} title={label} on:click={() => hidden.set(false)}><span></span></button>
  {/if}
  <div class="body" hidden={$hidden && !peek}><slot /></div>
</div>

<style>
  .host.off { flex: 1; width: 100%; }
  .host.off .body { flex: 1; }
  .host { flex: none; display: flex; min-height: 0; height: 100%; }
  .host .body { display: flex; min-height: 0; height: 100%; }
  .host .body[hidden] { display: none; }
  .host.hidden { position: relative; width: 12px; background: var(--surface-1); border-right: 1px solid var(--border); }
  .strip { position: absolute; inset: 0; width: 12px; padding: 0; border: none; border-radius: 0; background: transparent; cursor: pointer; display: grid; place-items: center; }
  .strip:hover:not(:disabled) { background: var(--hover); border: none; }
  .strip span { width: 3px; height: 32px; border-radius: 3px; background: var(--border-strong); }
  .strip:hover span { background: var(--accent); }
  .host.hidden .body { position: absolute; left: 0; top: 0; bottom: 0; z-index: 30; box-shadow: var(--shadow-lg); animation: ph-in var(--dur-slow) var(--ease); }
  @keyframes ph-in { from { transform: translateX(-24px); opacity: 0; } to { transform: none; opacity: 1; } }
  @media (prefers-reduced-motion: reduce) { .host.hidden .body { animation: none; } }
</style>
