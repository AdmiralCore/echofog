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
  import Button from './ui/Button.svelte';
  import IconButton from './ui/IconButton.svelte';
  import Input from './ui/Input.svelte';
  import Badge from './ui/Badge.svelte';
  import Toggle from './ui/Toggle.svelte';

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
    <Badge variant={st.peers > 0 ? 'ok' : 'warn'} dot>{st.peers > 0 ? $t('boot.peers', { n: st.peers }) : $t('boot.noPeers')}</Badge>
  </div>

  <form class="addrow" on:submit|preventDefault={add}>
    <div class="grow"><Input bind:el={field} bind:value={input} spellcheck="false" placeholder={v2 ? 'echofog://join?v=2&card=…' : 'echofog://relay/1.2.3.4:4001/…'} aria-label={$t('boot.inputLabel')} /></div>
    <Button type="submit" variant="primary" loading={busy} disabled={!input.trim()}>{$t('boot.add')}</Button>
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
            <Badge variant={n.connected ? 'ok' : 'neutral'}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</Badge>
            <Button size="sm" variant="ghost" loading={checking[keyOf(n)]} on:click={() => check(n)}>{checking[keyOf(n)] ? $t('boot.checking') : $t('boot.check')}</Button>
            {#if checked[keyOf(n)] !== undefined}
              <Badge variant={checked[keyOf(n)] ? 'ok' : 'danger'}>{checked[keyOf(n)] ? $t('boot.reachable') : $t('boot.unreachable')}</Badge>
            {/if}
          {:else}
            <Badge variant={n.connected ? 'ok' : 'neutral'}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</Badge>
            <IconButton icon={copied === (n.peerId || n.link) ? 'check' : 'copy'} size="sm" label={$t('boot.copyLink')} on:click={() => copyLink(n)} />
          {/if}
          <IconButton icon="trash" size="sm" variant="danger" label={$t('common.remove')} on:click={() => remove(n)} />
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
            <span class="sw" title={$t('boot.enable')}><Toggle checked={n.enabled} label={$t('boot.enable') + ': ' + label(n)} on:change={(e) => setEnabled(n, e.detail)} /></span>
          {:else}
            <span class="dot" class:on={n.connected}></span>
          {/if}
          <span class="grow addr" title={n.link || label(n)}><span class="mono">{label(n)}</span>{#if n.peerId && n.addr} <span class="faint mono">/{short(n.peerId, 4)}</span>{/if}</span>
          {#if v2}
            {#if !n.enabled}
              <span class="small faint">{$t('boot.disabled')}</span>
            {:else}
              <Badge variant={n.connected ? 'ok' : 'neutral'}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</Badge>
              <Button size="sm" variant="ghost" loading={checking[keyOf(n)]} on:click={() => check(n)}>{checking[keyOf(n)] ? $t('boot.checking') : $t('boot.check')}</Button>
              {#if checked[keyOf(n)] !== undefined}
                <Badge variant={checked[keyOf(n)] ? 'ok' : 'danger'}>{checked[keyOf(n)] ? $t('boot.reachable') : $t('boot.unreachable')}</Badge>
              {/if}
            {/if}
          {:else}
            <Badge variant={n.connected ? 'ok' : 'neutral'}>{n.connected ? $t('boot.connected') : $t('boot.notConnected')}</Badge>
          {/if}
        </div>
      {/each}
    </div>
  {:else}
    <p class="small faint">{$t('boot.noBuiltIn')}</p>
  {/if}
</div>

<style>
  .addrow { display: flex; gap: var(--sp-2); align-items: flex-start; }
  .nodes { display: flex; flex-direction: column; gap: 4px; }
  .node { display: flex; align-items: center; gap: var(--sp-2); padding: var(--sp-2) var(--sp-2) var(--sp-2) var(--sp-3); border-radius: var(--r-lg); background: var(--surface-2); }
  .node.off { opacity: 0.55; }
  /* the switch is named by aria-label; its visible label would repeat the address next to it */
  .sw :global(.txt) { display: none; }
  .sw { flex: none; }
  .addr { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .addr .mono { word-break: normal; }
</style>
