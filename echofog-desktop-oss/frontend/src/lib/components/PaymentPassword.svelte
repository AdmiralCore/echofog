<script>
  // Takes the app password before a payment, when this session asks it (stores/paygate.js).
  import { App } from '../api.js';
  import { payPrompt, closePayPrompt } from '../stores/paygate.js';
  import Modal from './ui/Modal.svelte';
  import Icon from './ui/Icon.svelte';
  import { t } from '../i18n/index.js';

  let password = '';
  let error = '';
  let busy = false;

  $: if (!$payPrompt) { password = ''; error = ''; }

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
    <form class="pay stack" on:submit|preventDefault={confirm}>
      <div class="card-head">
        <span class="ico"><Icon name="lock" size={19} /></span>
        <div class="grow">
          <h2>{$t('remember.payTitle')}</h2>
          <p class="small muted">{$t('remember.payText')}</p>
        </div>
      </div>
      <!-- svelte-ignore a11y-autofocus -->
      <label>{$t('remember.password')}<input type="password" bind:value={password} autocomplete="off" autofocus /></label>
      {#if error}<p class="small error">{error}</p>{/if}
      <div class="row">
        <button type="button" class="grow" on:click={() => closePayPrompt(false)} disabled={busy}>{$t('common.cancel')}</button>
        <button type="submit" class="primary grow" disabled={busy || !password}>{$t('remember.payConfirm')}</button>
      </div>
    </form>
  </Modal>
{/if}

<style>
  .pay { padding: 22px; }
</style>
