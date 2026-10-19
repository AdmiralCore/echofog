<script>
  // The one dialog the app shows for confirmations (see stores/dialog.js). Enter confirms,
  // Escape or a click outside cancels; focus starts on the safe answer for destructive actions.
  import { tick } from 'svelte';
  import { fade, scale } from 'svelte/transition';
  import { dialog, answer } from '../../stores/dialog.js';
  import Icon from './Icon.svelte';

  let okBtn;
  let cancelBtn;

  $: if ($dialog) focusFirst($dialog.danger);
  async function focusFirst(danger) {
    await tick();
    (danger ? cancelBtn : okBtn)?.focus();
  }

  function onKey(e) {
    if (!$dialog) return;
    if (e.key === 'Escape') { e.preventDefault(); answer(false); }
    if (e.key === 'Enter' && document.activeElement !== cancelBtn) { e.preventDefault(); answer(true); }
  }
</script>

<svelte:window on:keydown={onKey} />

{#if $dialog}
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" in:fade={{ duration: 140 }} on:click|self={() => answer(false)}>
    <div class="dlg" role="alertdialog" aria-modal="true" aria-labelledby="dlg-msg" in:scale={{ start: 0.94, duration: 180 }}>
      <div class="head">
        <span class="ico" class:danger={$dialog.danger}><Icon name={$dialog.danger ? 'trash' : 'info'} size={18} /></span>
        <div class="text">
          {#if $dialog.title}<h3>{$dialog.title}</h3>{/if}
          <p id="dlg-msg">{$dialog.message}</p>
        </div>
      </div>
      <div class="actions">
        <button bind:this={cancelBtn} class="ghost" on:click={() => answer(false)}>{$dialog.cancel}</button>
        <button bind:this={okBtn} class={$dialog.danger ? 'danger' : 'primary'} on:click={() => answer(true)}>{$dialog.confirm}</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed; inset: 0; z-index: 1000; display: grid; place-items: center; padding: 16px;
    background: rgba(8, 10, 18, 0.45); backdrop-filter: blur(4px);
  }
  .dlg {
    width: min(440px, 100%); padding: 22px; border-radius: var(--radius-xl, 22px);
    background: var(--glass, var(--bg)); border: 1px solid var(--line-soft);
    box-shadow: var(--shadow-lg); backdrop-filter: blur(24px) saturate(1.4);
    display: flex; flex-direction: column; gap: 18px;
  }
  .head { display: flex; gap: 14px; align-items: flex-start; }
  .ico {
    width: 38px; height: 38px; flex: none; border-radius: 12px; display: grid; place-items: center;
    background: var(--accent-soft); color: var(--accent);
  }
  .ico.danger { background: var(--danger-soft); color: var(--danger); }
  .text { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
  h3 { margin: 0; font-size: 16px; }
  p { margin: 0; line-height: 1.45; color: var(--text); white-space: pre-line; word-break: break-word; }
  .actions { display: flex; justify-content: flex-end; gap: 8px; }
  .actions button { min-width: 96px; height: 38px; }
  .ghost { background: var(--raised); color: var(--text); border: 1px solid var(--line-soft); }
  .danger { background: var(--danger); color: #fff; border: none; }
  .danger:hover:not(:disabled) { filter: brightness(1.08); background: var(--danger); }
</style>
