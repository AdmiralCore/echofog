<script>
  // Who may use the exit of this computer (T10.9): the link, the people connected now, the people who
  // tried and wait for a decision, the confirmed ones, and the open access for a limited time.
  import { createEventDispatcher } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { when } from '../format.js';
  import { t, lang } from '../i18n/index.js';

  export let access = null;
  export let enabled = false;

  const dispatch = createEventDispatcher();
  let busy = '';
  let error = '';
  let minutes = 60;
  let labels = {};

  const run = async (key, fn) => {
    busy = key;
    error = '';
    try {
      await fn();
      dispatch('change');
    } catch (e) {
      error = errText(e);
    } finally {
      busy = '';
    }
  };

  const short = (id) => id.slice(0, 8);
  $: open = access && access.anyUntil > 0;
</script>

{#if access && enabled}
  <div class="sub stack" data-testid="exit-access">
    <div class="eyebrow">{$t('proxy.xa.link')}</div>
    {#if access.link}
      <div class="row">
        <code class="mono grow small ellipsis">{access.link}</code>
        <button class="tiny" on:click={() => copy(access.link)}>{$t('common.copy')}</button>
      </div>
    {/if}
    <p class="small muted">{$t('proxy.xa.how')}</p>

    <div class="eyebrow">{$t('proxy.xa.connected')}</div>
    {#each access.clients || [] as c (c.id)}
      <div class="row small">
        <span class="grow">{c.label || (c.byLink ? $t('proxy.xa.byLink') : short(c.id))}</span>
        <span class="faint">{$t('proxy.xa.streams', { n: c.streams })}</span>
      </div>
    {:else}
      <p class="small faint">{$t('proxy.xa.nobody')}</p>
    {/each}

    {#if (access.waiting || []).length}
      <div class="eyebrow">{$t('proxy.xa.waiting')}</div>
      <p class="small muted">{$t('proxy.xa.waitingHint')}</p>
      {#each access.waiting as w (w.id)}
        <div class="row wrap small">
          <span class="mono">{short(w.id)}</span>
          <span class="faint grow">{$t('proxy.xa.tried', { n: w.tries, when: when(w.last, $lang) })}</span>
          <input class="name" placeholder={$t('proxy.xa.name')} bind:value={labels[w.id]} maxlength="64" />
          <button class="primary tiny" disabled={!!busy} on:click={() => run('ok' + w.id, () => App.SharingApprove(w.id, labels[w.id] || ''))}>{$t('proxy.xa.approve')}</button>
          <button class="tiny" disabled={!!busy} on:click={() => run('no' + w.id, () => App.SharingReject(w.id))}>{$t('proxy.xa.reject')}</button>
        </div>
      {/each}
    {/if}

    {#if (access.confirmed || []).length}
      <div class="eyebrow">{$t('proxy.xa.confirmed')}</div>
      {#each access.confirmed as k (k.id)}
        <div class="row small">
          <span class="grow">{k.label || short(k.id)}</span>
          <button class="tiny" disabled={!!busy} on:click={() => run('rv' + k.id, () => App.SharingRevoke(k.id))}>{$t('proxy.xa.revoke')}</button>
        </div>
      {/each}
    {/if}

    <div class="eyebrow">{$t('proxy.xa.open')}</div>
    {#if open}
      <div class="row small">
        <span class="grow">{$t('proxy.xa.openUntil', { when: when(access.anyUntil, $lang) })}</span>
        <button class="danger tiny" disabled={!!busy} on:click={() => run('close', () => App.SharingCloseOpen())}>{$t('proxy.xa.close')}</button>
      </div>
    {:else}
      <p class="small muted">{$t('proxy.xa.openHint')}</p>
      <div class="row small">
        <label class="grow">{$t('proxy.xa.minutes')}<input type="number" min="1" max="10080" bind:value={minutes} /></label>
        <button class="tiny" disabled={!!busy} on:click={() => run('open', () => App.SharingOpenFor(Math.round(Number(minutes) || 0)))}>{$t('proxy.xa.openFor')}</button>
      </div>
    {/if}
    {#if error}<p class="small error">{error}</p>{/if}
  </div>
{/if}

<style>
  .name { width: 9rem; }
  .ellipsis { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
