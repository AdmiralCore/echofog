<script>
  // "Remember on this computer": the profile opens without its password for this Windows user.
  // Switching it on, or changing whether payments ask the password, takes the password; switching
  // it off does not. Not shown where the system cannot keep it (only Windows can yet).
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import { lock } from '../stores/session.js';
  import { closeChats } from '../stores/chats.js';
  import { endCall } from '../stores/call.store.js';
  import Icon from './ui/Icon.svelte';
  import { t } from '../i18n/index.js';

  let st = null; // what the Go side says (RememberedUnlockDTO)
  let want = false; // the switch
  let ask = true; // ask the password before payments: on by default
  let password = '';
  let busy = false;
  let error = '';
  let notice = '';

  async function load() {
    try {
      st = await App.GetRememberedUnlock();
      want = st.enabled;
      ask = st.enabled ? st.askPayments : true;
    } catch (err) {
      error = errText(err);
    }
  }

  $: needsPassword = st && want && (!st.enabled || ask !== st.askPayments);

  async function switched() {
    error = notice = '';
    password = '';
    if (want || !st.enabled) return; // switching on waits for the password
    try {
      await App.ForgetUnlock();
      notice = $t('remember.forgotten');
    } catch (err) {
      error = errText(err);
    }
    await load();
  }

  async function save() {
    busy = true;
    error = notice = '';
    try {
      await App.RememberUnlock(password, ask);
      password = '';
      notice = $t('settings.savedNow');
      await load();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  async function lockNow() {
    await endCall();
    closeChats();
    await lock();
  }

  onMount(load);
</script>

{#if st?.supported}
  <div class="card stack">
    <div class="card-head">
      <span class="ico" class:warn={st.enabled}><Icon name="lockOpen" size={19} /></span>
      <div class="grow">
        <h2>{$t('remember.title')}</h2>
        <p class="small muted">{$t('remember.hint')}</p>
      </div>
      <input type="checkbox" class="switch" aria-label={$t('remember.title')} bind:checked={want} on:change={switched} disabled={busy} />
    </div>
    <p class="small warn">{$t('remember.warn')}</p>

    {#if want}
      <label class="row opt">
        <input type="checkbox" bind:checked={ask} disabled={busy} />
        <span class="grow">
          {$t('remember.askPayments')}
          <span class="small muted block">{$t('remember.askPaymentsHint')}</span>
        </span>
      </label>
      {#if needsPassword}
        <form class="row" on:submit|preventDefault={save}>
          <input class="grow" type="password" bind:value={password} placeholder={$t('remember.password')} aria-label={$t('remember.password')} autocomplete="off" />
          <button class="primary" type="submit" disabled={busy || !password}>{st.enabled ? $t('common.save') : $t('remember.enable')}</button>
        </form>
        <p class="small faint">{$t('remember.passwordWhy')}</p>
      {/if}
      {#if st.enabled}
        <div class="row lockrow">
          <button class="with-icon" on:click={lockNow}><Icon name="lock" size={16} /> {$t('remember.lock')}</button>
          <span class="small muted grow">{$t('remember.lockHint')}</span>
        </div>
      {/if}
    {/if}
    {#if error}<p class="small error">{error}</p>{/if}
    {#if notice}<p class="small ok">{notice}</p>{/if}
  </div>
{/if}

<style>
  .opt { align-items: flex-start; }
  .opt input { margin-top: 3px; }
  .block { display: block; margin-top: 2px; }
  .lockrow { flex-wrap: wrap; }
</style>
