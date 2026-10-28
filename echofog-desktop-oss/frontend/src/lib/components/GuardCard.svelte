<script>
  /**
   * How this app joins the EchoFog network. In guard mode it goes through a few camouflaged guard
   * servers instead of dialing the mesh itself, so its address is not spread around. On by default.
   * This does not change the VPN exit (that is on the Proxy page); it is about how the app itself
   * talks to the network.
   */
  import { onMount, onDestroy } from 'svelte';
  import { App, errText } from '../api.js';
  import { short } from '../format.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  let st = { mode: 'auto', active: false, connected: 0, wanted: 0, guards: [] };
  let error = '';
  let busy = false;
  let timer;

  async function load() {
    try { st = await App.GetGuardStatus(); error = ''; } catch (_) { /* node not started */ }
  }

  async function setMode(mode) {
    if (busy || mode === st.mode) return;
    busy = true;
    error = '';
    try {
      await App.SetGuardMode(mode);
      await load();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  onMount(() => {
    load();
    timer = setInterval(load, 4000);
  });
  onDestroy(() => clearInterval(timer));

  const MODES = ['auto', 'on', 'off'];
</script>

<div class="card stack">
  <div class="card-head">
    <span class="ico"><Icon name="shield" size={19} /></span>
    <div class="grow">
      <h2>{$t('guard.title')}</h2>
      <p class="small muted">{$t('guard.hint')}</p>
    </div>
  </div>
  {#if error}<p class="small error">{error}</p>{/if}

  <div class="modes" role="radiogroup" aria-label={$t('guard.title')}>
    {#each MODES as m}
      <button class="mode" class:sel={st.mode === m} role="radio" aria-checked={st.mode === m}
              disabled={busy} on:click={() => setMode(m)}>
        <span class="name">{$t('guard.mode.' + m)}</span>
        <span class="small muted">{$t('guard.mode.' + m + 'Hint')}</span>
      </button>
    {/each}
  </div>

  {#if st.active}
    <div class="status">
      {#if st.connected > 0}
        <span class="dot ok"></span>
        {$t('guard.through', { n: st.connected })}
      {:else}
        <span class="dot wait"></span>
        {$t('guard.connecting')}
      {/if}
    </div>
    {#if st.guards.length}
      <ul class="guards">
        {#each st.guards as g}
          <li><span class="dot" class:ok={g.connected}></span><span class="mono grow">{short(g.peerId)}</span></li>
        {/each}
      </ul>
    {/if}
  {:else}
    <p class="small muted">{$t('guard.offNote')}</p>
  {/if}

  <p class="small muted note">{$t('guard.exitNote')}</p>
</div>

<style>
  .modes { display: flex; gap: 8px; flex-wrap: wrap; }
  .mode {
    flex: 1 1 140px; display: flex; flex-direction: column; gap: 2px; align-items: flex-start;
    padding: 10px 12px; border: 1px solid var(--line); border-radius: 10px; background: var(--surface);
    cursor: pointer; text-align: left;
  }
  .mode.sel { border-color: var(--accent); background: var(--accent-weak, var(--surface)); }
  .mode .name { font-weight: 600; }
  .status { display: flex; align-items: center; gap: 8px; }
  .guards { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
  .guards li { display: flex; align-items: center; gap: 8px; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--muted); flex: none; }
  .dot.ok { background: #38c172; }
  .dot.wait { background: #e0a106; }
  .note { border-top: 1px solid var(--line); padding-top: 8px; }
</style>
