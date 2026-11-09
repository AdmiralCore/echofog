<script>
  // Which protocol the node runs on (T11.7): 1 (the default) or 2. The choice applies at the next start.
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import Icon from './ui/Icon.svelte';
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
    <label class="row opt">
      <input type="radio" name="protocol" value={1} bind:group={want} on:change={set} disabled={busy} />
      <span class="grow">{$t('protocol.v1')}</span>
    </label>
    <label class="row opt">
      <input type="radio" name="protocol" value={2} bind:group={want} on:change={set} disabled={busy} />
      <span class="grow">{$t('protocol.v2')}</span>
    </label>
    <p class="small muted">{$t('protocol.running', { n: st.running })}</p>
    {#if st.next && st.next !== st.running}
      <p class="small">{$t('protocol.pending', { n: st.next })}</p>
    {/if}
    {#if want === 2}<p class="small warn">{$t('protocol.warn2')}</p>{/if}
    {#if saved}<p class="small ok">{$t('protocol.saved')}</p>{/if}
    {#if error}<p class="small error">{error}</p>{/if}
  </div>
{/if}
