<script>
  /**
   * Nodes the app joins the network through. When the built-in ones are blocked (or a build ships
   * none), the user pastes a node address from a friend or the project's channel; it is saved and
   * dialed at once, and again whenever the app is left without peers.
   */
  import { onMount, onDestroy, tick } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { focus } from '../stores/nav.js';
  import { short } from '../format.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  let st = { builtIn: [], custom: [], peers: 0 };
  let input = '';
  let busy = false;
  let error = '';
  let added = '';
  let copied = '';
  let card;
  let field;
  let timer;

  async function load() {
    try { st = await App.GetBootstrapNodes(); } catch (_) { /* node not started */ }
  }

  async function add() {
    if (!input.trim() || busy) return;
    busy = true;
    error = added = '';
    try {
      await App.AddBootstrapNode(input.trim());
      input = '';
      added = $t('boot.added');
      setTimeout(() => (added = ''), 4000);
      await load();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  async function remove(n) {
    error = '';
    try { await App.RemoveBootstrapNode(n.addr + '/' + n.peerId); await load(); } catch (err) { error = errText(err); }
  }

  async function copyLink(n) {
    if (await copy(n.link)) {
      copied = n.peerId;
      setTimeout(() => (copied = ''), 1500);
    }
  }

  onMount(async () => {
    await load();
    timer = setInterval(load, 3000);
    // the offline banner sends people here: bring the card into view and focus the field
    if ($focus === 'bootstrap') {
      focus.set('');
      await tick();
      card?.scrollIntoView({ behavior: 'smooth', block: 'center' });
      setTimeout(() => field?.focus(), 350);
    }
  });
  onDestroy(() => clearInterval(timer));
</script>

<div class="card stack" bind:this={card}>
  <div class="card-head">
    <span class="ico" class:ok={st.peers > 0} class:warn={st.peers === 0}><Icon name="link" size={18} /></span>
    <div class="grow">
      <h2>{$t('boot.title')}</h2>
      <p class="small muted">{$t('boot.hint')}</p>
    </div>
    <span class="chip" class:ok={st.peers > 0} class:warn={st.peers === 0}>{st.peers > 0 ? $t('boot.peers', { n: st.peers }) : $t('boot.noPeers')}</span>
  </div>

  <form class="row" on:submit|preventDefault={add}>
    <input class="grow" bind:this={field} bind:value={input} spellcheck="false" placeholder="echofog://relay/1.2.3.4:4001/…" aria-label={$t('boot.inputLabel')} />
    <button class="primary" type="submit" disabled={busy || !input.trim()}>{busy ? $t('common.working') : $t('boot.add')}</button>
  </form>
  <p class="small faint">{$t('boot.format')}</p>
  {#if error}<p class="small error">{error}</p>{/if}
  {#if added}<p class="small ok">{added}</p>{/if}

  {#if st.custom.length}
    <div class="eyebrow">{$t('boot.yours')}</div>
    <div class="nodes">
      {#each st.custom as n (n.addr + n.peerId)}
        <div class="node">
          <span class="dot" class:on={n.connected} title={n.connected ? $t('boot.connected') : $t('boot.notConnected')}></span>
          <span class="grow addr"><span class="mono">{n.addr}</span> <span class="faint mono">/{short(n.peerId, 4)}</span></span>
          <span class="small" class:ok={n.connected} class:faint={!n.connected}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</span>
          <button class="icon tiny" title={$t('boot.copyLink')} aria-label={$t('boot.copyLink')} on:click={() => copyLink(n)}><Icon name={copied === n.peerId ? 'check' : 'copy'} size={14} /></button>
          <button class="icon tiny del" title={$t('common.remove')} aria-label={$t('common.remove')} on:click={() => remove(n)}><Icon name="trash" size={14} /></button>
        </div>
      {/each}
    </div>
  {/if}

  <div class="eyebrow">{$t('boot.builtIn')}</div>
  {#if st.builtIn.length}
    <div class="nodes">
      {#each st.builtIn as n (n.addr + n.peerId)}
        <div class="node">
          <span class="dot" class:on={n.connected}></span>
          <span class="grow addr"><span class="mono">{n.addr}</span></span>
          <span class="small" class:ok={n.connected} class:faint={!n.connected}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</span>
        </div>
      {/each}
    </div>
  {:else}
    <p class="small faint">{$t('boot.noBuiltIn')}</p>
  {/if}
</div>

<style>
  .nodes { display: flex; flex-direction: column; gap: 4px; }
  .node { display: flex; align-items: center; gap: 10px; padding: 8px 8px 8px 12px; border-radius: 12px; background: var(--raised); animation: fade-up 0.25s var(--ease) both; }
  .addr { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .addr .mono { word-break: normal; }
  .icon.tiny { width: 28px; height: 28px; border-radius: 8px; }
  .del:hover:not(:disabled) { color: var(--danger); background: var(--danger-soft); }
</style>
