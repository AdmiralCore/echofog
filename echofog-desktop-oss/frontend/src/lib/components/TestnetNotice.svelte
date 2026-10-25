<script>
  // Shown wherever balances or payments appear, only when the wallet is on the TON testnet.
  // The faucet hint can be hidden for this run; the "not real money" warning always stays.
  import { onMount } from 'svelte';
  import { openURL } from '../api.js';
  import Icon from './ui/Icon.svelte';
  import { t } from '../i18n/index.js';
  import { tonNetwork, faucetHidden, loadTonNetwork } from '../stores/tonnet.js';

  export let compact = false; // one line, no faucet hint: for small blocks inside other cards

  const FAUCET = 'https://t.me/testgiver_ton_bot';

  onMount(loadTonNetwork);
</script>

{#if $tonNetwork === 'testnet'}
  <div class="tn small" class:compact role="note">
    <div class="row warn-line">
      <Icon name="info" size={15} />
      <span class="grow"><b>{$t('testnet.title')}</b> {$t('testnet.notReal')}</span>
    </div>
    {#if !compact && !$faucetHidden}
      <div class="faucet">
        <span>{$t('testnet.faucet')}</span>
        <div class="row wrap">
          <button class="small with-icon" on:click={() => openURL(FAUCET)}><Icon name="external" size={14} /> @testgiver_ton_bot</button>
          <button class="ghost small" on:click={() => faucetHidden.set(true)}>{$t('testnet.gotIt')}</button>
        </div>
      </div>
    {/if}
  </div>
{/if}

<style>
  .tn { display: flex; flex-direction: column; gap: 8px; padding: 10px 14px; border-radius: var(--radius); background: var(--warn-soft); border-left: 3px solid var(--warn); line-height: 1.45; }
  .tn.compact { padding: 7px 10px; border-radius: 10px; }
  .warn-line { gap: 8px; align-items: flex-start; }
  .warn-line :global(svg) { color: var(--warn); flex: none; margin-top: 2px; }
  .warn-line b { color: var(--warn); }
  .faucet { display: flex; flex-direction: column; gap: 8px; color: var(--muted); padding-left: 23px; }
</style>
