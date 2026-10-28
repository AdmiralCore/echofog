<script>
  import { acceptRequest, dismissRequest, blockPeer, unblockPeer } from '../../stores/chats.js';
  import { errText } from '../../api.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Icon from '../../components/ui/Icon.svelte';

  /** Above the composer of a direct chat: a blocked person, or a stranger who wrote first. */
  export let conv;

  let name = '';
  let busy = false;
  let error = '';
  async function run(fn) {
    busy = true;
    error = '';
    try { await fn(); } catch (err) { error = errText(err); } finally { busy = false; }
  }
  async function block() {
    if (!(await ask({ message: $t('chats.confirm.block', { name: conv.name }), confirm: $t('chats.block'), danger: true }))) return;
    await run(() => blockPeer(conv));
  }
</script>

{#if conv.blocked}
  <div class="reqbar">
    <p class="small"><Icon name="lock" size={14} /> {$t('chats.blockedNote')}</p>
    <div class="row wrap"><button class="small" on:click={() => run(() => unblockPeer(conv))} disabled={busy}>{$t('chats.unblock')}</button></div>
  </div>
{:else if conv.request}
  <div class="reqbar">
    <p class="small"><b>{$t('chats.requestHead')}</b> {$t('chats.requestNote')}</p>
    <div class="row wrap">
      <input class="grow" bind:value={name} placeholder={$t('chats.requestName')} maxlength="64" />
      <button class="primary small" on:click={() => run(() => acceptRequest(conv, name))} disabled={busy}>{$t('chats.acceptRequest')}</button>
      <button class="small" on:click={() => run(() => dismissRequest(conv))} disabled={busy}>{$t('chats.dismissRequest')}</button>
      <button class="danger small" on:click={block} disabled={busy}>{$t('chats.block')}</button>
    </div>
    {#if error}<p class="small error">{error}</p>{/if}
  </div>
{/if}
