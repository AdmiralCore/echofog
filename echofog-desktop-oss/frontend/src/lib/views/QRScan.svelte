<script>
  import { onMount, onDestroy, createEventDispatcher } from 'svelte';
  import { Html5Qrcode } from 'html5-qrcode';

  const dispatch = createEventDispatcher();
  let reader;
  let error = '';
  let running = false;

  onMount(async () => {
    reader = new Html5Qrcode('qr-reader');
    try {
      await reader.start(
        { facingMode: 'environment' },
        { fps: 10, qrbox: { width: 220, height: 220 } },
        (text) => {
          if (!running) return;
          running = false;
          reader.stop().then(() => dispatch('scan', text)).catch(() => dispatch('scan', text));
        },
        () => {},
      );
      running = true;
    } catch (err) {
      error = (err && (err.message || String(err))) || 'The camera is not available.';
    }
  });

  onDestroy(() => {
    if (reader && running) reader.stop().then(() => reader.clear()).catch(() => {});
  });
</script>

<div class="scan stack tight">
  <div id="qr-reader" />
  {#if error}
    <p class="error small">Could not use the camera: {error}</p>
  {:else}
    <p class="small muted">Point the camera at the contact's code.</p>
  {/if}
  <button on:click={() => dispatch('close')}>Cancel</button>
</div>

<style>
  .scan { padding: 4px 12px 12px; }
  #qr-reader { width: 100%; min-height: 200px; background: #000; border-radius: var(--radius); overflow: hidden; }
  :global(#qr-reader video) { width: 100% !important; object-fit: cover; }
  :global(#qr-reader__dashboard_section_csr), :global(#qr-reader__dashboard_section_swaplink) { display: none !important; }
</style>
