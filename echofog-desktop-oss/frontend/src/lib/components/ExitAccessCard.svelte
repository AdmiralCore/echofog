<script>
  // Who may use the exit of this computer (T10.9): the link, the people connected now, the people who
  // tried and wait for a decision, the confirmed ones, and the open access for a limited time.
  import { createEventDispatcher } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { when } from '../format.js';
  import { t, lang } from '../i18n/index.js';
  import Button from './ui/Button.svelte';
  import IconButton from './ui/IconButton.svelte';
  import Input from './ui/Input.svelte';
  import Badge from './ui/Badge.svelte';

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
      <div class="linkbox">
        <code class="mono grow small ellipsis">{access.link}</code>
        <IconButton icon="copy" size="sm" label={$t('common.copy')} on:click={() => copy(access.link)} />
      </div>
    {/if}
    <p class="small muted">{$t('proxy.xa.how')}</p>

    <div class="eyebrow">{$t('proxy.xa.connected')}</div>
    {#each access.clients || [] as c (c.id)}
      <div class="line">
        <span class="grow">{c.label || (c.byLink ? $t('proxy.xa.byLink') : short(c.id))}</span>
        <Badge>{$t('proxy.xa.streams', { n: c.streams })}</Badge>
      </div>
    {:else}
      <p class="small faint">{$t('proxy.xa.nobody')}</p>
    {/each}

    {#if (access.waiting || []).length}
      <div class="eyebrow">{$t('proxy.xa.waiting')}</div>
      <p class="small muted">{$t('proxy.xa.waitingHint')}</p>
      {#each access.waiting as w (w.id)}
        <div class="line wrap">
          <span class="mono">{short(w.id)}</span>
          <span class="faint small grow">{$t('proxy.xa.tried', { n: w.tries, when: when(w.last, $lang) })}</span>
          <div class="name"><Input size="sm" aria-label={$t('proxy.xa.name')} placeholder={$t('proxy.xa.name')} bind:value={labels[w.id]} maxlength="64" /></div>
          <Button size="sm" variant="primary" disabled={!!busy} on:click={() => run('ok' + w.id, () => App.SharingApprove(w.id, labels[w.id] || ''))}>{$t('proxy.xa.approve')}</Button>
          <Button size="sm" disabled={!!busy} on:click={() => run('no' + w.id, () => App.SharingReject(w.id))}>{$t('proxy.xa.reject')}</Button>
        </div>
      {/each}
    {/if}

    {#if (access.confirmed || []).length}
      <div class="eyebrow">{$t('proxy.xa.confirmed')}</div>
      {#each access.confirmed as k (k.id)}
        <div class="line">
          <span class="grow">{k.label || short(k.id)}</span>
          <Button size="sm" disabled={!!busy} on:click={() => run('rv' + k.id, () => App.SharingRevoke(k.id))}>{$t('proxy.xa.revoke')}</Button>
        </div>
      {/each}
    {/if}

    <div class="eyebrow">{$t('proxy.xa.open')}</div>
    {#if open}
      <div class="line">
        <span class="grow small">{$t('proxy.xa.openUntil', { when: when(access.anyUntil, $lang) })}</span>
        <Button size="sm" variant="danger" disabled={!!busy} on:click={() => run('close', () => App.SharingCloseOpen())}>{$t('proxy.xa.close')}</Button>
      </div>
    {:else}
      <p class="small muted">{$t('proxy.xa.openHint')}</p>
      <div class="line wrap">
        <div class="mins"><Input type="number" size="sm" label={$t('proxy.xa.minutes')} min="1" max="10080" bind:value={minutes} /></div>
        <Button size="sm" disabled={!!busy} loading={busy === 'open'} on:click={() => run('open', () => App.SharingOpenFor(Math.round(Number(minutes) || 0)))}>{$t('proxy.xa.openFor')}</Button>
      </div>
    {/if}
    {#if error}<p class="small error" role="alert">{error}</p>{/if}
  </div>
{/if}

<style>
  .line { display: flex; align-items: center; gap: var(--sp-2); min-height: 32px; font-size: var(--fs-sm); }
  .line.wrap { flex-wrap: wrap; }
  .line .grow { flex: 1 1 auto; min-width: 0; }
  .linkbox { display: flex; align-items: center; gap: var(--sp-2); padding: var(--sp-2) var(--sp-3); border: 1px solid var(--border); border-radius: var(--r-md); background: var(--surface-1); }
  .name { width: 9rem; }
  .mins { width: 8rem; }
  .ellipsis { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
