<script>
  /**
   * Anonymous mode: who may see this computer's address. Through Tor nobody in the network does;
   * through the user's own relay only that relay does, and the node connects to nothing else. Calls
   * and files go through relays, nothing punches holes or maps ports, the app's own lookups go
   * through the proxy or not at all, and the TON wallet stays offline unless allowed. A mode that
   * cannot hold (Tor with no proxy, no relay pinned) connects nowhere. It applies on the next start.
   */
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import { anon, refreshAnon } from '../stores/anon.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  const MODES = ['', 'tor', 'relay'];
  let mode = '';
  let wallet = false;
  let error = '';
  let busy = false;
  let saved = false;

  $: st = $anon;
  $: if (st && !touched) { mode = st.mode || ''; wallet = !!st.walletDirect; }
  let touched = false;
  $: dirty = st && (mode !== (st.mode || '') || wallet !== !!st.walletDirect);
  $: needs = mode === 'tor' && st && !st.hasProxy ? 'tor' : mode === 'relay' && st && !st.hasRelay ? 'relay' : '';

  function pick(m) {
    mode = m;
    touched = true;
    saved = false;
    error = '';
  }
  async function save() {
    busy = true;
    error = '';
    try {
      await App.SetAnonymity(mode, wallet);
      await refreshAnon();
      touched = false;
      saved = true;
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }
  onMount(refreshAnon);
</script>

<div class="card stack">
  <div class="card-head">
    <span class="ico" class:on={st && st.running}><Icon name="eyeOff" size={19} /></span>
    <div class="grow">
      <h2>{$t('anon.title')}</h2>
      <p class="small muted">{$t('anon.hint')}</p>
    </div>
  </div>

  {#if st}
    <p class="now" class:hidden={st.seenBy !== 'network'}>
      <Icon name={st.seenBy === 'network' ? 'eye' : 'shield'} size={15} />
      {#if st.broken}{$t('anon.now.broken', { why: st.broken })}
      {:else}{$t('anon.now.' + st.seenBy)}{/if}
    </p>
  {/if}

  <div class="opts" role="radiogroup" aria-label={$t('anon.title')}>
    {#each MODES as m}
      <button class="opt" class:sel={mode === m} role="radio" aria-checked={mode === m} on:click={() => pick(m)}>
        <span class="name">{$t('anon.mode.' + (m || 'off'))}</span>
        <span class="small muted">{$t('anon.mode.' + (m || 'off') + 'Hint')}</span>
      </button>
    {/each}
  </div>

  {#if needs}<p class="small warn">{$t('anon.needs.' + needs)}</p>{/if}

  {#if mode}
    <ul class="small muted what">
      <li>{$t('anon.what.calls')}</li>
      <li>{$t('anon.what.nat')}</li>
      <li>{$t('anon.what.lookups')}</li>
      {#if mode === 'relay'}<li>{$t('anon.what.relayOnly')}</li>{/if}
    </ul>
    <label class="check">
      <input type="checkbox" bind:checked={wallet} on:change={() => { touched = true; saved = false; }} />
      <span class="small">{$t('anon.wallet')}</span>
    </label>
    <p class="small faint">{$t('anon.limits')}</p>
  {/if}

  {#if error}<p class="small error">{error}</p>{/if}
  <div class="row">
    <button class="primary" disabled={busy || !dirty || !!needs} on:click={save}>{$t('anon.save')}</button>
    {#if st && st.restart}
      <span class="small muted">{$t('anon.restart')}</span>
    {:else if saved}
      <span class="small muted">{$t('anon.saved')}</span>
    {/if}
  </div>
</div>

<style>
  .opts { display: flex; gap: 8px; flex-wrap: wrap; }
  .opt {
    flex: 1 1 150px; display: flex; flex-direction: column; gap: 2px; align-items: flex-start;
    padding: 10px 12px; border: 1px solid var(--line); border-radius: 10px; background: var(--surface);
    cursor: pointer; text-align: left;
  }
  .opt.sel { border-color: var(--accent); background: var(--accent-weak, var(--surface)); }
  .opt .name { font-weight: 600; }
  .row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  .ico.on { color: #38c172; }
  .now { display: flex; align-items: center; gap: 8px; margin: 0; padding: 8px 10px; border-radius: 10px; font-size: 13px; background: var(--raised); color: var(--muted); }
  .now.hidden { color: #38c172; }
  .what { margin: 0; padding-left: 18px; display: flex; flex-direction: column; gap: 3px; }
  .check { display: flex; align-items: center; gap: 8px; }
  .check input { width: auto; }
  .warn { color: #c98a00; }
</style>
