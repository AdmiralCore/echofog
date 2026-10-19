<script>
  // Privacy: who may write first and who may call, and the people this user blocked.
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import { short, when } from '../format.js';
  import { loadChats } from '../stores/chats.js';
  import Icon from './ui/Icon.svelte';
  import { t, lang } from '../i18n/index.js';

  let privacy = null;
  let blocked = [];
  let error = '';
  let busy = '';
  let saved = false;

  async function load() {
    try {
      privacy = await App.GetPrivacy();
      blocked = (await App.GetBlockedPeers()) || [];
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }

  async function set(patch) {
    busy = 'save';
    error = '';
    saved = false;
    try {
      await App.SetPrivacy({ ...privacy, ...patch });
      privacy = { ...privacy, ...patch };
      saved = true;
      setTimeout(() => (saved = false), 1500);
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }

  async function unblock(id) {
    busy = id;
    try {
      await App.UnblockPeer(id);
      await load();
      await loadChats();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }

  onMount(load);
</script>

<div class="card stack">
  <div class="card-head">
    <span class="ico"><Icon name="lock" size={19} /></span>
    <div class="grow">
      <h2>{$t('settings.privacy')}</h2>
      <p class="small muted">{$t('settings.privacyHint')}</p>
    </div>
  </div>
  {#if error}<p class="small error">{error}</p>{/if}

  {#if privacy}
    <label>{$t('settings.whoMessages')}
      <select value={privacy.messages} disabled={!!busy} on:change={(e) => set({ messages: e.currentTarget.value })}>
        <option value="everyone">{$t('settings.who.everyoneRequests')}</option>
        <option value="contacts">{$t('settings.who.contacts')}</option>
      </select>
    </label>
    <label>{$t('settings.whoCalls')}
      <select value={privacy.calls} disabled={!!busy} on:change={(e) => set({ calls: e.currentTarget.value })}>
        <option value="contacts">{$t('settings.who.contacts')}</option>
        <option value="everyone">{$t('settings.who.everyone')}</option>
        <option value="nobody">{$t('settings.who.nobody')}</option>
      </select>
    </label>
    <p class="small faint">{$t('settings.groupCallsNote')}{#if saved} <span class="ok">{$t('settings.savedNow')}</span>{/if}</p>

    <div class="sub">
      <div class="eyebrow">{$t('settings.blocked')}</div>
      {#each blocked as b (b.peer_id)}
        <div class="peer-row">
          <span class="grow">{#if b.alias}{b.alias} <span class="faint mono">· {short(b.peer_id)}</span>{:else}<span class="mono">{short(b.peer_id)}</span>{/if}
            <span class="small faint">· {when(b.at, $lang)}</span></span>
          <button class="small" disabled={!!busy} on:click={() => unblock(b.peer_id)}>{$t('chats.unblock')}</button>
        </div>
      {:else}
        <p class="small faint">{$t('settings.blockedEmpty')}</p>
      {/each}
    </div>
  {:else if !error}
    <p class="small faint row"><span class="spinner"></span> {$t('common.loading')}</p>
  {/if}
</div>

<style>
  .sub { display: flex; flex-direction: column; gap: 8px; }
  .peer-row { display: flex; align-items: center; gap: 10px; padding: 8px 10px; border-radius: 12px; background: var(--raised); }
</style>
