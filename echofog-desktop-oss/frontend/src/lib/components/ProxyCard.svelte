<script>
  /**
   * Tor and other SOCKS5 proxies: the app's own connections (messages, the network) go out through
   * the proxy, over TCP. Calls are chosen apart: Tor carries no UDP and is too slow for video, so a
   * call can take a direct link, at the price of the other side seeing your address. Both settings
   * apply when the app next starts.
   */
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  const PRESETS = { torBrowser: '127.0.0.1:9150', tor: '127.0.0.1:9050' };
  const ROUTES = ['auto', 'direct', 'relay'];

  let st = null;
  let kind = 'none'; // none | torBrowser | tor | custom
  let custom = '';
  let route = 'auto';
  let error = '';
  let busy = '';
  let check = ''; // '' | 'ok' | error text
  let saved = false;

  $: addr = kind === 'none' ? '' : kind === 'custom' ? custom.trim() : PRESETS[kind];
  $: dirty = st && (addr !== st.proxy || route !== st.callRoute);

  function fromStatus(s) {
    const p = s.proxy || '';
    kind = !p ? 'none' : p === PRESETS.torBrowser ? 'torBrowser' : p === PRESETS.tor ? 'tor' : 'custom';
    custom = kind === 'custom' ? p : custom;
    route = s.callRoute || 'auto';
  }

  async function load() {
    try {
      st = await App.GetProxySettings();
      fromStatus(st);
      error = '';
    } catch (_) { /* node not started */ }
  }

  async function save() {
    busy = 'save';
    error = '';
    saved = false;
    try {
      await App.SetProxySettings(addr, route);
      st = await App.GetProxySettings();
      saved = true;
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }

  async function test() {
    if (!addr) return;
    busy = 'check';
    check = '';
    try {
      await App.CheckProxy(addr);
      check = 'ok';
    } catch (err) {
      check = errText(err);
    } finally {
      busy = '';
    }
  }

  onMount(load);
</script>

<div class="card stack">
  <div class="card-head">
    <span class="ico"><Icon name="globe" size={19} /></span>
    <div class="grow">
      <h2>{$t('netproxy.title')}</h2>
      <p class="small muted">{$t('netproxy.hint')}</p>
    </div>
  </div>
  {#if error}<p class="small error">{error}</p>{/if}

  <div class="opts" role="radiogroup" aria-label={$t('netproxy.title')}>
    {#each ['none', 'torBrowser', 'tor', 'custom'] as k}
      <button class="opt" class:sel={kind === k} role="radio" aria-checked={kind === k}
              on:click={() => { kind = k; check = ''; saved = false; }}>
        <span class="name">{$t('netproxy.kind.' + k)}</span>
        <span class="small muted">{k === 'custom' ? $t('netproxy.kind.customHint') : k === 'none' ? $t('netproxy.kind.noneHint') : PRESETS[k]}</span>
      </button>
    {/each}
  </div>

  {#if kind === 'custom'}
    <input type="text" bind:value={custom} placeholder="127.0.0.1:1080" on:input={() => { check = ''; saved = false; }} />
  {/if}

  {#if kind !== 'none'}
    <div class="row">
      <button disabled={busy !== '' || !addr} on:click={test}>{busy === 'check' ? $t('netproxy.checking') : $t('netproxy.check')}</button>
      {#if check === 'ok'}
        <span class="small ok-text"><span class="dot ok"></span>{$t('netproxy.checkOk')}</span>
      {:else if check}
        <span class="small error">{check}</span>
      {/if}
    </div>
    {#if kind === 'torBrowser' || kind === 'tor'}
      <p class="small muted">{$t('netproxy.torNote')}</p>
    {/if}
  {/if}

  <div class="calls note">
    <h3 class="small">{$t('netproxy.calls.title')}</h3>
    <div class="opts" role="radiogroup" aria-label={$t('netproxy.calls.title')}>
      {#each ROUTES as r}
        <button class="opt" class:sel={route === r} role="radio" aria-checked={route === r}
                on:click={() => { route = r; saved = false; }}>
          <span class="name">{$t('netproxy.calls.' + r)}</span>
          <span class="small muted">{$t('netproxy.calls.' + r + 'Hint')}</span>
        </button>
      {/each}
    </div>
    {#if kind !== 'none' && route === 'direct'}
      <p class="small warn">{$t('netproxy.calls.directWarn')}</p>
    {/if}
  </div>

  <div class="row">
    <button class="primary" disabled={busy !== '' || !dirty} on:click={save}>{$t('netproxy.save')}</button>
    {#if st && st.restart}
      <span class="small muted">{$t('netproxy.restart')}</span>
    {:else if saved}
      <span class="small muted">{$t('netproxy.saved')}</span>
    {/if}
  </div>

  {#if st}
    <p class="small muted note">
      {st.running ? $t('netproxy.now.on', { addr: st.running }) : $t('netproxy.now.off')}
      · {$t('netproxy.now.calls', { route: $t('netproxy.calls.' + st.runningCallRoute) })}
    </p>
  {/if}
  {#if addr}
    <p class="small faint">{$t('netproxy.limits')}</p>
  {/if}
</div>

<style>
  .opts { display: flex; gap: 8px; flex-wrap: wrap; }
  .opt {
    flex: 1 1 140px; display: flex; flex-direction: column; gap: 2px; align-items: flex-start;
    padding: 10px 12px; border: 1px solid var(--line); border-radius: 10px; background: var(--surface);
    cursor: pointer; text-align: left;
  }
  .opt.sel { border-color: var(--accent); background: var(--accent-weak, var(--surface)); }
  .opt .name { font-weight: 600; }
  .row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  .calls h3 { margin: 0 0 6px; font-weight: 600; }
  .note { border-top: 1px solid var(--line); padding-top: 8px; }
  .dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; }
  .dot.ok { background: #38c172; }
  .warn { color: #c98a00; }
</style>
