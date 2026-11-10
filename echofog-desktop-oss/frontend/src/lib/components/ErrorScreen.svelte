<script>
  // A full-screen error of a failed action (T11.5): a link made by another version, a server
  // without a cover, the free volume of the day used up. The core names it by code; the words are
  // the app's own (lib/i18n/ns/errscreen.js), the core's Russian text stands in for an unknown code.
  import { fade, scale } from 'svelte/transition';
  import { errorScreen, screenKeys } from '../errscreen.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';
  import Button from './ui/Button.svelte';

  $: keys = $errorScreen ? screenKeys($errorScreen.code) : null;
  const close = () => errorScreen.set(null);
</script>

{#if $errorScreen}
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" transition:fade={{ duration: 140 }} on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
    <div class="dlg" role="alertdialog" aria-modal="true" aria-labelledby="es-title" aria-describedby="es-text" transition:scale={{ start: 0.97, duration: 160 }}>
      <div class="head">
        <span class="ico"><Icon name="info" size={18} /></span>
        <div class="text">
          <h3 id="es-title">{keys ? $t(keys.title) : $errorScreen.title}</h3>
          <p id="es-text">{keys ? $t(keys.text) : $errorScreen.text}</p>
        </div>
      </div>
      <div class="actions">
        <Button variant="primary" on:click={close}>{$t('errscreen.close')}</Button>
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop { position: fixed; inset: 0; z-index: var(--z-dialog); display: grid; place-items: center; padding: 16px; background: var(--scrim); }
  .dlg {
    width: min(440px, 100%); padding: 22px; border-radius: var(--r-xl); display: flex; flex-direction: column; gap: 18px;
    background: var(--surface-1); border: 1px solid var(--border-strong); box-shadow: var(--shadow-xl);
  }
  .head { display: flex; gap: 14px; align-items: flex-start; }
  .ico { width: 36px; height: 36px; flex: none; border-radius: var(--r-lg); display: grid; place-items: center; background: var(--accent-soft); color: var(--accent-fg); }
  .text { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
  h3 { margin: 0; font-size: var(--fs-lg); line-height: var(--lh-lg); }
  p { margin: 0; font-size: var(--fs-md); line-height: var(--lh-md); color: var(--text); word-break: break-word; }
  .actions { display: flex; justify-content: flex-end; }
</style>
