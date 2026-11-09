<script>
  /**
   * Nodes the app joins the network through: "Настройки › Сеть › Узлы входа" (T5.6).
   * Protocol 2 shows the signed built-in list (each entry can be switched off) and the user's own
   * entries (a link or a T5.5 string). "Проверить" asks the node whether it answers; nothing about
   * the address goes to the log.
   */
  import { onMount, onDestroy, tick } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { focus } from '../stores/nav.js';
  import { short } from '../format.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  let st = { builtIn: [], custom: [], peers: 0, protocol: 1 };
  let input = '';
  let busy = false;
  let error = '';
  let added = '';
  let copied = '';
  let checking = {};
  let checked = {};
  let card;
  let field;
  let timer;
  let v2 = false;

  function keyOf(n) {
    return (n.index ? 'b' + n.index : '') + (n.link || n.addr || n.peerId);
  }

  async function load() {
    try {
      st = await App.GetBootstrapNodes();
      v2 = st.protocol === 2;
    } catch (_) { /* node not started */ }
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

  function removeEntry(n) {
    // v2 keeps the whole list line; v1 keeps host:port/peer id
    return n.link || (n.addr + '/' + n.peerId);
  }

  async function remove(n) {
    error = '';
    try { await App.RemoveBootstrapNode(removeEntry(n)); await load(); } catch (err) { error = errText(err); }
  }

  async function setEnabled(n, on) {
    error = '';
    try { await App.SetBootstrapNodeEnabled(n.index, on); await load(); } catch (err) { error = errText(err); }
  }

  async function check(n) {
    const k = keyOf(n);
    checking = { ...checking, [k]: true };
    error = '';
    try {
      const ok = await App.CheckBootstrapNode(n.link || n.addr);
      checked = { ...checked, [k]: ok };
    } catch (err) {
      error = errText(err);
    } finally {
      checking = { ...checking, [k]: false };
    }
  }

  async function copyLink(n) {
    if (await copy(n.link)) {
      copied = n.peerId || n.link;
      setTimeout(() => (copied = ''), 1500);
    }
  }

  function label(n) {
    if (n.addr) return n.addr;
    if (n.peerId) return $t('boot.card') + ' ' + short(n.peerId, 4);
    return $t('boot.entry');
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
    <input class="grow" bind:this={field} bind:value={input} spellcheck="false" placeholder={v2 ? 'echofog://join?v=2&card=…' : 'echofog://relay/1.2.3.4:4001/…'} aria-label={$t('boot.inputLabel')} />
    <button class="primary" type="submit" disabled={busy || !input.trim()}>{busy ? $t('common.working') : $t('boot.add')}</button>
  </form>
  <p class="small faint">{$t(v2 ? 'boot.formatV2' : 'boot.format')}</p>
  {#if error}<p class="small error">{error}</p>{/if}
  {#if added}<p class="small ok">{added}</p>{/if}

  {#if st.custom.length}
    <div class="eyebrow">{$t('boot.yours')}</div>
    <div class="nodes">
      {#each st.custom as n (keyOf(n))}
        <div class="node">
          <span class="dot" class:on={n.connected} title={n.connected ? $t('boot.connected') : $t('boot.notConnected')}></span>
          <span class="grow addr" title={n.link || label(n)}><span class="mono">{label(n)}</span>{#if n.peerId && n.addr} <span class="faint mono">/{short(n.peerId, 4)}</span>{/if}</span>
          {#if v2}
            <span class="small" class:ok={n.connected} class:faint={!n.connected}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</span>
            <button class="linky small" type="button" disabled={checking[keyOf(n)]} on:click={() => check(n)}>
              {#if checking[keyOf(n)]}{$t('boot.checking')}{:else}{$t('boot.check')}{/if}
            </button>
            {#if checked[keyOf(n)] !== undefined}
              <span class="small" class:ok={checked[keyOf(n)]} class:error={!checked[keyOf(n)]}>{checked[keyOf(n)] ? $t('boot.reachable') : $t('boot.unreachable')}</span>
            {/if}
          {:else}
            <span class="small" class:ok={n.connected} class:faint={!n.connected}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</span>
            <button class="icon tiny" title={$t('boot.copyLink')} aria-label={$t('boot.copyLink')} on:click={() => copyLink(n)}><Icon name={copied === (n.peerId || n.link) ? 'check' : 'copy'} size={14} /></button>
          {/if}
          <button class="icon tiny del" title={$t('common.remove')} aria-label={$t('common.remove')} on:click={() => remove(n)}><Icon name="trash" size={14} /></button>
        </div>
      {/each}
    </div>
  {/if}

  <div class="eyebrow">{$t('boot.builtIn')}</div>
  {#if st.builtIn.length}
    <div class="nodes">
      {#each st.builtIn as n (keyOf(n))}
        <div class="node" class:off={!n.enabled}>
          {#if v2}
            <input type="checkbox" checked={n.enabled} aria-label={$t('boot.enable')} on:change={(e) => setEnabled(n, e.currentTarget.checked)} />
          {:else}
            <span class="dot" class:on={n.connected}></span>
          {/if}
          <span class="grow addr" title={n.link || label(n)}><span class="mono">{label(n)}</span>{#if n.peerId && n.addr} <span class="faint mono">/{short(n.peerId, 4)}</span>{/if}</span>
          {#if v2}
            {#if !n.enabled}
              <span class="small faint">{$t('boot.disabled')}</span>
            {:else}
              <span class="small" class:ok={n.connected} class:faint={!n.connected}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</span>
              <button class="linky small" type="button" disabled={checking[keyOf(n)]} on:click={() => check(n)}>
                {#if checking[keyOf(n)]}{$t('boot.checking')}{:else}{$t('boot.check')}{/if}
              </button>
              {#if checked[keyOf(n)] !== undefined}
                <span class="small" class:ok={checked[keyOf(n)]} class:error={!checked[keyOf(n)]}>{checked[keyOf(n)] ? $t('boot.reachable') : $t('boot.unreachable')}</span>
              {/if}
            {/if}
          {:else}
            <span class="small" class:ok={n.connected} class:faint={!n.connected}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</span>
          {/if}
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
  .node.off { opacity: 0.55; }
  .addr { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .addr .mono { word-break: normal; }
  .icon.tiny { width: 28px; height: 28px; border-radius: 8px; }
  .del:hover:not(:disabled) { color: var(--danger); background: var(--danger-soft); }
  .linky { border: 0; background: none; padding: 0 4px; cursor: pointer; color: var(--accent); }
  .linky:disabled { opacity: 0.6; cursor: default; }
</style>
