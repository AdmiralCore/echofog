<script>
  import { onMount } from 'svelte';
  import { screen, boot } from './lib/stores/session.js';
  import Auth from './lib/views/Auth.svelte';
  import Shell from './lib/views/Shell.svelte';

  onMount(boot);
</script>

{#if $screen === 'app'}
  <Shell />
{:else if $screen === 'loading'}
  <div class="loading">
    <span class="ring" aria-hidden="true"></span>
    <span class="muted small">Starting…</span>
  </div>
{:else}
  <Auth />
{/if}

<style>
  .loading { height: 100%; display: flex; flex-direction: column; gap: 16px; align-items: center; justify-content: center; animation: fade 0.4s ease-out; }
  .ring {
    width: 38px; height: 38px; border-radius: 50%;
    border: 3px solid var(--accent-soft); border-top-color: var(--accent);
    animation: spin 0.9s linear infinite;
  }
</style>
