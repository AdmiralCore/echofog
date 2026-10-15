<script>
  // The user's deposit for paid nodes (the TON vault). Nodes take what the app signed for
  // every few hours; taking the rest back takes 24 hours. See docs/vault-payments.md.
  import { onMount, onDestroy, createEventDispatcher } from 'svelte';
  import { App, errText, openURL } from '../api.js';
  import { amount } from '../format.js';
  import Icon from './ui/Icon.svelte';
  import { t, lang } from '../i18n/index.js';

  export let explorer = 'https://tonviewer.com';
  export let balanceTon = null; // wallet TON balance, for a friendlier check before sending

  const dispatch = createEventDispatcher();

  let vault = null;
  let loadError = '';
  let amt = '';
  let busy = '';
  let error = '';
  let notice = '';
  let confirmWithdraw = false;
  let nowMs = Date.now();
  let timer, tick, fast;

  $: first = vault && !vault.deployed;
  $: hasDeposit = vault?.deployed && Number(vault.depositTon) > 0;
  $: left = vault?.closing ? Math.max(0, vault.withdrawAt * 1000 - nowMs) : 0;

  function countdown(ms, tt) {
    const m = Math.ceil(ms / 60000);
    const h = Math.floor(m / 60);
    return h ? tt('vault.countdownHm', { h, m: String(m % 60).padStart(2, '0') }) : tt('vault.countdownM', { m });
  }

  async function refresh() {
    try {
      vault = await App.GetVault();
      loadError = '';
    } catch (err) {
      loadError = errText(err);
    }
  }

  // after a transaction the vault changes within a minute or so: look more often for a while
  function watchSoon() {
    clearInterval(fast);
    let n = 0;
    fast = setInterval(() => {
      refresh();
      dispatch('changed');
      if (++n >= 12) clearInterval(fast);
    }, 5000);
  }

  async function act(name, fn, done) {
    busy = name;
    error = notice = '';
    try {
      await fn();
      notice = done;
      watchSoon();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }

  function deposit() {
    error = notice = '';
    const s = String(amt).trim().replace(',', '.');
    const n = Number(s);
    if (!(n > 0)) return (error = $t('wallet.errAmount'));
    if (first && n < Number(vault.minFirstTon)) return (error = $t('vault.errFirstMin', { min: vault.minFirstTon }));
    if (balanceTon != null && n + 0.02 > Number(balanceTon)) return (error = $t('vault.errOverBalance'));
    act('deposit', async () => {
      await App.VaultDeposit(s);
      amt = '';
    }, $t('vault.sentDeposit'));
  }

  const withdraw = () => act('withdraw', async () => {
    await App.VaultRequestWithdraw();
    confirmWithdraw = false;
  }, $t('vault.withdrawRequested'));
  const cancel = () => act('cancel', () => App.VaultCancelWithdraw(), $t('vault.withdrawCancelled'));
  const finish = () => act('finish', () => App.VaultExecuteWithdraw(), $t('vault.withdrawSent'));

  onMount(() => {
    refresh();
    timer = setInterval(refresh, 30000);
    tick = setInterval(() => (nowMs = Date.now()), 15000);
  });
  onDestroy(() => {
    clearInterval(timer);
    clearInterval(tick);
    clearInterval(fast);
  });
</script>

{#if vault}
  <div class="card stack">
    <div class="card-head">
      <span class="ico" class:ok={hasDeposit && !vault.closing}><Icon name="lock" size={18} /></span>
      <div class="grow">
        <h2>{$t('vault.title')} {#if vault.closing}<span class="chip warn head-chip">{$t('vault.withdrawing')}</span>{/if}</h2>
        <p class="small muted">{$t('vault.hint')}</p>
      </div>
    </div>

    <div class="dep">
      <div class="eyebrow">{$t('vault.deposit')}</div>
      {#if first}
        <div class="dep-amt none">{$t('vault.none')}</div>
      {:else}
        <div class="dep-amt">{amount(vault.depositTon, $lang)} <span>TON</span></div>
      {/if}
    </div>

    {#if vault.closing}
      <div class="closing">
        {#if vault.canFinish}
          <p><b>{$t('vault.readyBold')}</b> {$t('vault.readyAfter')}</p>
        {:else}
          <p>{$t('vault.backIn')} <b>{countdown(left, $t)}</b>{$t('vault.backInAfter')}</p>
        {/if}
      </div>
      <div class="row wrap">
        <button class="primary" on:click={finish} disabled={!vault.canFinish || !!busy}>{busy === 'finish' ? $t('vault.sending') : $t('vault.finish')}</button>
        <button on:click={cancel} disabled={!!busy}>{busy === 'cancel' ? $t('vault.sending') : $t('vault.cancel')}</button>
      </div>
    {:else}
      <div class="row wrap end">
        <label class="amt">{first ? $t('vault.amountLabel') : $t('vault.addLabel')}
          <input bind:value={amt} placeholder={first ? vault.minFirstTon : '1'} inputmode="decimal" on:keydown={(e) => e.key === 'Enter' && deposit()} />
        </label>
        <button class="primary" on:click={deposit} disabled={!!busy}>{busy === 'deposit' ? $t('vault.sending') : $t('vault.depositBtn')}</button>
        {#if hasDeposit && !confirmWithdraw}
          <button on:click={() => { confirmWithdraw = true; notice = error = ''; }} disabled={!!busy}>{$t('vault.withdraw')}</button>
        {/if}
      </div>
      {#if first}
        <p class="small faint">{$t('vault.firstMin', { min: vault.minFirstTon })}</p>
      {/if}
      {#if confirmWithdraw}
        <div class="confirm stack">
          <p class="small">{$t('vault.confirmBefore')} <b>{$t('vault.confirm24h')}</b> {$t('vault.confirmAfter')}</p>
          <div class="row">
            <button class="danger" on:click={withdraw} disabled={!!busy}>{busy === 'withdraw' ? $t('vault.sending') : $t('vault.startWithdraw')}</button>
            <button on:click={() => (confirmWithdraw = false)} disabled={!!busy}>{$t('common.back')}</button>
          </div>
        </div>
      {/if}
    {/if}

    {#if notice}<p class="ok small">{notice}</p>{/if}
    {#if error}<p class="error small">{error}</p>{/if}
    {#if loadError}<p class="faint small">{$t('vault.couldNotRefresh', { err: loadError })}</p>{/if}

    <details>
      <summary class="small muted">{$t('vault.details')}</summary>
      <div class="stack details">
        <p class="small muted">{$t('vault.detailsText')}</p>
        <div class="mono small">{vault.address}</div>
        <p class="small"><a href={`${explorer}/${vault.address}`} on:click|preventDefault={() => openURL(`${explorer}/${vault.address}`)}>{$t('wallet.viewExplorer')}</a></p>
      </div>
    </details>
  </div>
{/if}

<style>
  .head-chip { margin-left: 6px; vertical-align: middle; font-size: 11px; }
  .dep { display: flex; flex-direction: column; gap: 2px; }
  .dep-amt { font-size: 26px; font-weight: 720; letter-spacing: -0.02em; font-variant-numeric: tabular-nums; }
  .dep-amt.none { font-size: 17px; font-weight: 600; color: var(--muted); letter-spacing: 0; }
  .dep-amt span { font-size: 15px; color: var(--muted); font-weight: 600; }
  .end { align-items: flex-end; }
  .amt { width: 180px; }
  .amt input { font-variant-numeric: tabular-nums; }
  .closing { padding: 12px 14px; border-radius: 14px; background: var(--warn-soft); }
  .closing p { margin: 0; }
  .confirm { padding: 14px; border-radius: 14px; background: var(--raised); animation: fade-up 0.3s var(--ease); }
  .confirm p { margin: 0; }
  .details { margin-top: 10px; gap: 8px; }
  .details p { margin: 0; }
</style>
