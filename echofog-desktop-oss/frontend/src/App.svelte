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
  <div class="loading muted">Starting…</div>
{:else}
  <Auth />
{/if}

<style>
  .loading { height: 100%; display: grid; place-items: center; }
</style>
