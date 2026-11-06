<script>
  // A full-screen error of a failed action (T11.5): a link made by another version, a server
  // without a cover, the free volume of the day used up. The core names it by code; the words are
  // the app's own (lib/i18n/ns/errscreen.js), the core's Russian text stands in for an unknown code.
  import { fade, scale } from 'svelte/transition';
  import { errorScreen, screenKeys } from '../errscreen.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  $: keys = $errorScreen ? screenKeys($errorScreen.code) : null;
  const close = () => errorScreen.set(null);
</script>

{#if $errorScreen}
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" transition:fade={{ duration: 140 }} on:click|self={close}>
    <div class="dlg" role="alertdialog" aria-modal="true" aria-labelledby="es-title" aria-describedby="es-text" transition:scale={{ start: 0.95, duration: 180 }}>
      <span class="ico"><Icon name="info" size={22} /></span>
      <h3 id="es-title">{keys ? $t(keys.title) : $errorScreen.title}</h3>
      <p id="es-text">{keys ? $t(keys.text) : $errorScreen.text}</p>
      <button class="primary" on:click={close}>{$t('errscreen.close')}</button>
    </div>
  </div>
{/if}

<style>
  .backdrop { position: fixed; inset: 0; z-index: 10002; display: grid; place-items: center; padding: 16px; background: var(--scrim); backdrop-filter: blur(6px); }
  .dlg {
    width: min(420px, 100%); display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 24px 22px 20px; text-align: center;
    border-radius: 20px; background: var(--panel); border: 1px solid var(--line); box-shadow: var(--shadow-lg);
  }
  .ico { width: 44px; height: 44px; border-radius: 14px; display: grid; place-items: center; background: var(--accent-soft); color: var(--accent); }
  h3 { margin: 4px 0 0; font-size: 17px; }
  p { margin: 0 0 8px; color: var(--muted, inherit); line-height: 1.5; }
</style>
