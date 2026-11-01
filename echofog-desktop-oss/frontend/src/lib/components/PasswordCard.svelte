<script>
  // Changing the app password. It takes the current one; a profile remembered on this computer
  // stays remembered under the new password (the Go side seals it again).
  import { App, errText } from '../api.js';
  import Icon from './ui/Icon.svelte';
  import { t } from '../i18n/index.js';

  let current = '';
  let next = '';
  let repeat = '';
  let busy = false;
  let error = '';
  let done = false;

  async function change() {
    error = '';
    done = false;
    if (next.length < 8) return (error = $t('auth.tooShort'));
    if (next !== repeat) return (error = $t('auth.mismatch'));
    busy = true;
    try {
      await App.ChangePassword(current, next);
      current = next = repeat = '';
      done = true;
    } catch (err) {
      error = /wrong app password/i.test(errText(err)) ? $t('remember.wrongPassword') : errText(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="card stack">
  <div class="card-head">
    <span class="ico"><Icon name="lock" size={19} /></span>
    <div class="grow">
      <h2>{$t('password.title')}</h2>
      <p class="small muted">{$t('password.hint')}</p>
    </div>
  </div>
  <form class="stack" on:submit|preventDefault={change}>
    <label>{$t('password.current')}<input type="password" bind:value={current} autocomplete="current-password" /></label>
    <label>{$t('password.new')}<input type="password" bind:value={next} autocomplete="new-password" /></label>
    <div class="meter" aria-hidden="true"><span style="width:{Math.min(100, next.length * 8)}%" class:good={next.length >= 12} class:ok={next.length >= 8 && next.length < 12}></span></div>
    <label>{$t('password.repeat')}<input type="password" bind:value={repeat} autocomplete="new-password" /></label>
    <div class="row">
      <button class="primary" type="submit" disabled={busy || !current || !next || !repeat}>
        {#if busy}<span class="spinner light"></span> {$t('password.changing')}{:else}{$t('password.change')}{/if}
      </button>
      {#if done}<span class="small ok">{$t('password.changed')}</span>{/if}
    </div>
  </form>
  {#if error}<p class="small error">{error}</p>{/if}
</div>

<style>
  .meter { height: 4px; border-radius: 4px; background: var(--raised); overflow: hidden; margin-top: -6px; }
  .meter span { display: block; height: 100%; background: var(--danger); border-radius: 4px; transition: width 0.3s var(--ease), background 0.3s; }
  .meter span.ok { background: var(--warn); }
  .meter span.good { background: var(--ok); }
  .spinner.light { border-color: rgba(255, 255, 255, 0.35); border-top-color: #fff; }
</style>
