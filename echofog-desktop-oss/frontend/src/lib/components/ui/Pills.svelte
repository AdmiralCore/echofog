<script>
  /** A choice among a few options as pills (a role, a term): the chosen one stands out, a role
   *  carries its color as a dot. */
  export let items = []; // [{ value, label, color, hint }]  color: a CSS color or ''
  export let value;
  export let label = '';
</script>

<div class="pills" role="radiogroup" aria-label={label}>
  {#each items as it (it.value)}
    <button type="button" role="radio" aria-checked={value === it.value} class:on={value === it.value} title={it.hint || ''}
      on:click={() => (value = it.value)}>
      {#if it.color}<span class="dot" style="background:{it.color}"></span>{/if}{it.label}
    </button>
  {/each}
</div>

<style>
  .pills { display: flex; flex-wrap: wrap; gap: 6px; }
  .pills button {
    display: inline-flex; align-items: center; gap: 6px; height: 30px; padding: 0 12px; border-radius: 99px;
    font-size: 12.5px; font-weight: 560; background: var(--raised); border: 1px solid var(--line-soft); color: var(--muted);
    transition: background var(--dur, 0.15s), color var(--dur, 0.15s), border-color var(--dur, 0.15s);
  }
  .pills button:hover:not(.on):not(:disabled) { color: var(--text); background: var(--hover); border-color: var(--line); }
  .pills button.on { background: var(--accent-soft); border-color: var(--accent); color: var(--accent); }
  .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; box-shadow: 0 0 0 2px rgba(255, 255, 255, 0.08); }
</style>
