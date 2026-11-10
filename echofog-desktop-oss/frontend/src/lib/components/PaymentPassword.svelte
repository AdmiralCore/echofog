<script>
  // Takes the app password before a payment, when this session asks it (stores/paygate.js).
  import { App } from '../api.js';
  import { payPrompt, closePayPrompt } from '../stores/paygate.js';
  import Modal from './ui/Modal.svelte';
  import Icon from './ui/Icon.svelte';
  import Button from './ui/Button.svelte';
  import Input from './ui/Input.svelte';
  import { t } from '../i18n/index.js';

  let password = '';
  let error = '';
  let busy = false;

  $: if (!$payPrompt) { password = ''; error = ''; }
  $: if ($payPrompt) setTimeout(() => document.querySelector('.pay input')?.focus(), 60);

  async function confirm() {
    busy = true;
    error = '';
    try {
      await App.AuthorizePayment(password);
      closePayPrompt(true);
    } catch (_) {
      error = $t('remember.wrongPassword');
    } finally {
      busy = false;
    }
  }
</script>

{#if $payPrompt}
  <Modal label={$t('remember.payTitle')} width={400} on:close={() => closePayPrompt(false)}>
    <form class="pay" on:submit|preventDefault={confirm}>
      <div class="head">
        <span class="ico"><Icon name="lock" size={16} /></span>
        <div class="grow">
          <h2>{$t('remember.payTitle')}</h2>
          <p class="small muted">{$t('remember.payText')}</p>
        </div>
      </div>
      <Input type="password" label={$t('remember.password')} bind:value={password} autocomplete="off" error={error} />
      <div class="row">
        <Button className="grow" on:click={() => closePayPrompt(false)} disabled={busy}>{$t('common.cancel')}</Button>
        <Button className="grow" variant="primary" type="submit" loading={busy} disabled={!password}>{$t('remember.payConfirm')}</Button>
      </div>
    </form>
  </Modal>
{/if}

<style>
  .pay { display: flex; flex-direction: column; gap: var(--sp-4); padding: var(--sp-5); }
  .head { display: flex; gap: var(--sp-3); align-items: flex-start; }
  .head h2 { font-size: var(--fs-md); line-height: var(--lh-md); }
  .head p { margin-top: 2px; line-height: 1.45; }
  .ico { width: 30px; height: 30px; border-radius: var(--r-md); flex: none; display: grid; place-items: center; background: var(--surface-2); color: var(--muted); }
</style>
