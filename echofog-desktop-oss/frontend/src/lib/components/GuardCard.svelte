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

  let st = { mode: 'auto', active: false, connected: 0, wanted: 0, guards: [], pinned: [] };
  let error = '';
  let busy = false;
  let timer;
  let pinEntry = '';
  let pinBusy = false;
  let pinError = '';

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

  async function pin() {
    const entry = pinEntry.trim();
    if (pinBusy || !entry) return;
    pinBusy = true;
    pinError = '';
    try {
      await App.PinGuard(entry);
      pinEntry = '';
      await load();
    } catch (err) {
      pinError = errText(err);
    } finally {
      pinBusy = false;
    }
  }

  async function unpin(p) {
    if (pinBusy) return;
    pinBusy = true;
    pinError = '';
    try {
      await App.UnpinGuard(p.link || (p.addr + '/' + p.peerId));
      await load();
    } catch (err) {
      pinError = errText(err);
    } finally {
      pinBusy = false;
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

  {#if st.stoodDown}
    <p class="small faint">{$t('guard.stoodDown', { cc: st.country })}</p>
  {/if}
  {#if st.active}
    <div class="status">
      {#if st.connected > 0}
        <span class="dot ok"></span>
        {$t('guard.through', { n: st.connected })}
      {:else}
        <span class="dot wait"></span>
        {$t('guard.connecting')}
      {/if}
      {#if st.mode === 'auto' && st.country}<span class="faint">· {$t('guard.censoring', { cc: st.country })}</span>{/if}
    </div>
    {#if st.guards.length}
      <ul class="guards">
        {#each st.guards as g}
          <li><span class="dot" class:ok={g.connected}></span><span class="mono grow">{short(g.peerId)}</span></li>
        {/each}
      </ul>
    {/if}
  {:else if !st.stoodDown}
    <p class="small muted">{$t('guard.offNote')}</p>
  {/if}

  <div class="pin note">
    <h3 class="small">{$t('guard.pin.title')}</h3>
    <p class="small muted">{$t('guard.pin.hint')}</p>
    {#if st.pinned.length}
      <ul class="pinned">
        {#each st.pinned as p}
          <li>
            <span class="dot" class:ok={st.guards.find((g) => g.peerId === p.peerId && g.connected)}></span>
            <span class="mono grow">{p.addr}</span>
            <button class="link" disabled={pinBusy} on:click={() => unpin(p)}>{$t('guard.pin.remove')}</button>
          </li>
        {/each}
      </ul>
    {/if}
    <div class="pin-add">
      <input type="text" bind:value={pinEntry} placeholder={$t('guard.pin.placeholder')}
             disabled={pinBusy} on:keydown={(e) => e.key === 'Enter' && pin()} />
      <button disabled={pinBusy || !pinEntry.trim()} on:click={pin}>{$t('guard.pin.add')}</button>
    </div>
    {#if pinError}<p class="small error">{pinError}</p>{/if}
  </div>

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
  .pin h3 { margin: 0 0 2px; font-weight: 600; }
  .pinned { list-style: none; margin: 8px 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
  .pinned li { display: flex; align-items: center; gap: 8px; }
  .pin-add { display: flex; gap: 8px; margin-top: 8px; }
  .pin-add input { flex: 1 1 auto; min-width: 0; }
  button.link {
    background: none; border: none; color: var(--accent); cursor: pointer; padding: 0;
    font: inherit; flex: none;
  }
  button.link:disabled { opacity: 0.5; cursor: default; }
</style>
