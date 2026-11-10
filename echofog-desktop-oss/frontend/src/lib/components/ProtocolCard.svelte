<script>
  // Which protocol the node runs on (T11.7): 1 (the default) or 2. The choice applies at the next start.
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import Icon from './ui/Icon.svelte';
  import Badge from './ui/Badge.svelte';
  import { t } from '../i18n/index.js';

  let st = null; // { running, next }
  let want = 1;
  let busy = false;
  let error = '';
  let saved = false;

  async function load() {
    try {
      st = await App.GetProtocol();
      want = st.next || st.running || 1;
    } catch (err) {
      error = errText(err);
    }
  }

  function pick(n) {
    if (busy || Number(want) === n) return;
    want = n;
    set();
  }

  async function set() {
    busy = true;
    error = '';
    saved = false;
    try {
      await App.SetProtocol(Number(want));
      saved = true;
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
    await load();
  }

  onMount(load);
</script>

{#if st}
  <div class="card stack">
    <div class="card-head">
      <span class="ico"><Icon name="globe" size={19} /></span>
      <div class="grow">
        <h2>{$t('protocol.title')}</h2>
        <p class="small muted">{$t('protocol.hint')}</p>
      </div>
    </div>
    <div class="opts" role="radiogroup" aria-label={$t('protocol.title')}>
      {#each [1, 2] as n}
        <button class="opt" class:sel={Number(want) === n} role="radio" aria-checked={Number(want) === n} disabled={busy} on:click={() => pick(n)}>
          <span class="name">{$t('protocol.v' + n)}</span>
          {#if st.running === n}<Badge variant="ok" dot>{$t('protocol.now')}</Badge>{/if}
        </button>
      {/each}
    </div>
    {#if st.next && st.next !== st.running}
      <p class="small">{$t('protocol.pending', { n: st.next })}</p>
    {/if}
    {#if Number(want) === 2}<p class="small warn">{$t('protocol.warn2')}</p>{/if}
    {#if saved}<p class="small ok" role="status">{$t('protocol.saved')}</p>{/if}
    {#if error}<p class="small error" role="alert">{error}</p>{/if}
  </div>
{/if}

<style>
  .opts { display: flex; gap: 8px; flex-wrap: wrap; }
  .opt {
    flex: 1 1 190px; display: flex; flex-direction: column; gap: 6px; align-items: flex-start;
    padding: 10px 12px; border: 1px solid var(--border-strong); border-radius: var(--r-lg); background: var(--surface-2);
    white-space: normal; height: auto; font-weight: var(--fw-regular); color: var(--text); cursor: pointer; text-align: left;
  }
  .opt.sel { border-color: var(--accent); background: var(--accent-soft); }
  .opt .name { font-weight: var(--fw-semibold); }
</style>
