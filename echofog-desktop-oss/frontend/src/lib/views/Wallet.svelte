<script>
  import { onMount, onDestroy } from 'svelte';
  import QRCode from 'qrcode';
  import { App, errText, copy, openURL } from '../api.js';
  import { amount, when, short } from '../format.js';

  let wallet = null;
  let balance = null;
  let history = [];
  let netError = '';
  let qr = '';
  let copied = false;
  let timer;

  let support = { available: false, percent: 0, pendingTon: '0', pendingUsdt: '0' };
  let onRamp = '';

  let send = { asset: 'USDT', to: '', amount: '', comment: '' };
  let step = 'form'; // form | review | sending | done
  let sendError = '';
  let sentHash = '';

  let exportPw = '';
  let words = [];
  let exportError = '';

  $: testnet = wallet?.network === 'testnet';
  $: explorer = testnet ? 'https://testnet.tonviewer.com' : 'https://tonviewer.com';
  $: available = send.asset === 'USDT' ? Number(balance?.usdt || 0) : Number(balance?.ton || 0);

  async function refresh() {
    try {
      if (!wallet) {
        wallet = await App.GetTonWallet();
        qr = await QRCode.toDataURL(wallet.topUpLink || wallet.address, { margin: 1, width: 160, color: { dark: '#000000', light: '#ffffff' } });
      }
      balance = await App.GetTonBalance();
      history = (await App.GetTonHistory(20)) || [];
      netError = '';
    } catch (err) {
      netError = errText(err);
    }
  }

  onMount(async () => {
    refresh();
    timer = setInterval(refresh, 15000);
    try { support = await App.GetSupport(); } catch (_) { /* hidden */ }
    try { onRamp = await App.GetOnRampURL(); } catch (_) { /* none */ }
  });
  onDestroy(() => clearInterval(timer));

  async function copyAddress() {
    copied = await copy(wallet.address);
    setTimeout(() => (copied = false), 1500);
  }

  function review() {
    sendError = '';
    const n = Number(String(send.amount).replace(',', '.'));
    if (!send.to.trim()) sendError = 'Enter the recipient address.';
    else if (!(n > 0)) sendError = 'Enter an amount greater than zero.';
    else if (balance && n > available) sendError = 'That is more than your balance.';
    else if (send.asset === 'USDT' && balance && Number(balance.ton) < 0.065) sendError = 'A USDT transfer needs about 0.06 TON for the network fee. Add a little TON first.';
    else step = 'review';
  }

  async function confirmSend() {
    step = 'sending';
    sendError = '';
    try {
      const fn = send.asset === 'USDT' ? App.SendUsdt : App.SendTon;
      sentHash = await fn(send.to.trim(), String(send.amount).replace(',', '.'), send.comment.trim());
      step = 'done';
      send = { ...send, to: '', amount: '', comment: '' };
      setTimeout(refresh, 6000);
    } catch (err) {
      sendError = errText(err);
      step = 'review';
    }
  }

  async function setShare(p) {
    try {
      await App.SetSupportPercent(p);
      support = await App.GetSupport();
    } catch (err) {
      netError = errText(err);
    }
  }

  async function exportPhrase() {
    exportError = '';
    try {
      words = (await App.ExportTonRecoveryPhrase(exportPw)) || [];
      exportPw = '';
    } catch (err) {
      exportError = errText(err);
    }
  }
</script>

<div class="page">
  <div class="stack">
    <h1>Wallet {#if testnet}<span class="small warn">testnet</span>{/if}</h1>

    <p class="note">
      This wallet is for paying for traffic and receiving payments for it. Do not keep savings here:
      the keys live on this computer and anyone who gets into it can spend them.
    </p>

    {#if netError}<p class="error small">{netError}</p>{/if}

    <div class="card row top">
      <div class="grow stack tight">
        <div class="small muted">Balance</div>
        <div class="balances">
          <div><span class="big">{balance ? amount(balance.usdt || 0) : '…'}</span> USDT</div>
          <div><span class="big">{balance ? amount(balance.ton) : '…'}</span> TON
            {#if balance?.usdRate}<span class="small faint">≈ {(Number(balance.ton) * balance.usdRate).toFixed(2)} USD</span>{/if}</div>
        </div>
        <div class="small muted" style="margin-top: 8px">Address</div>
        <div class="mono">{wallet?.address || '…'}</div>
        <div class="row">
          <button on:click={copyAddress} disabled={!wallet}>{copied ? 'Copied' : 'Copy address'}</button>
          {#if onRamp}<button on:click={() => openURL(onRamp)}>Buy USDT with a card</button>{/if}
        </div>
        <p class="small faint">
          USDT transfers need a little TON (about 0.06) for the network fee.
          {#if !onRamp}To get USDT, buy it on an exchange or Telegram Wallet and withdraw it to this address using the TON network.{/if}
        </p>
      </div>
      {#if qr}<img class="qr" src={qr} alt="Wallet address QR code" />{/if}
    </div>

    <div class="card stack">
      <h2>Send</h2>
      {#if step === 'form'}
        <div class="row wrap">
          <label class="asset">Asset
            <select bind:value={send.asset}><option>USDT</option><option>TON</option></select>
          </label>
          <label class="grow">Recipient address<input bind:value={send.to} spellcheck="false" /></label>
        </div>
        <div class="row wrap">
          <label class="asset">Amount<input bind:value={send.amount} /></label>
          <label class="grow">Comment (optional)<input bind:value={send.comment} /></label>
        </div>
        <div><button class="primary" on:click={review}>Review</button></div>
      {:else if step === 'review' || step === 'sending'}
        <p>Send <b>{send.amount} {send.asset}</b> to <span class="mono">{send.to}</span>{#if send.comment}, comment "{send.comment}"{/if}?</p>
        <p class="small muted">Blockchain transfers cannot be reversed.</p>
        <div class="row">
          <button class="primary" on:click={confirmSend} disabled={step === 'sending'}>{step === 'sending' ? 'Sending…' : 'Confirm'}</button>
          <button on:click={() => (step = 'form')} disabled={step === 'sending'}>Back</button>
        </div>
      {:else}
        <p class="ok">Sent. It shows up in the history once confirmed.</p>
        <p class="small"><a href={`${explorer}/transaction/${sentHash}`} on:click|preventDefault={() => openURL(`${explorer}/transaction/${sentHash}`)}>View in explorer</a></p>
        <div><button on:click={() => (step = 'form')}>New transfer</button></div>
      {/if}
      {#if sendError}<p class="error small">{sendError}</p>{/if}
    </div>

    <div class="card stack">
      <h2>History</h2>
      {#if history.length}
        <table class="small">
          <thead><tr><th>When</th><th>Amount</th><th>From / to</th><th>Comment</th></tr></thead>
          <tbody>
            {#each history as t (t.hash)}
              <tr class:failed={t.failed}>
                <td>{when(t.time)}</td>
                <td class:ok={t.incoming}>{t.incoming ? '+' : '-'}{amount(t.ton)} {t.asset || 'TON'}{t.failed ? ' (failed)' : ''}</td>
                <td class="mono">{short(t.counterparty)}</td>
                <td class="muted">{t.comment}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {:else}
        <p class="small muted">No transactions yet.</p>
      {/if}
    </div>

    {#if support.available}
      <div class="card stack">
        <h2>Support the project</h2>
        <p class="small muted">
          Optionally add a share on top of what you pay exits. It goes to the project that develops EchoFog.
          The exit still gets its full price.
        </p>
        <div class="row">
          {#each [0, 5, 10] as p}
            <button class:on={support.percent === p} on:click={() => setShare(p)}>{p === 0 ? 'Off' : p + '%'}</button>
          {/each}
        </div>
        {#if Number(support.pendingUsdt) > 0 || Number(support.pendingTon) > 0}
          <p class="small faint">Collected and sent in one batch later: {support.pendingUsdt} USDT, {support.pendingTon} TON.</p>
        {/if}
      </div>
    {/if}

    <details class="card">
      <summary class="muted">Wallet recovery phrase</summary>
      <div class="stack" style="margin-top: 12px">
        {#if words.length}
          <ol class="words">{#each words as w}<li class="mono">{w}</li>{/each}</ol>
          <div><button on:click={() => (words = [])}>Hide</button></div>
        {:else}
          <p class="small muted">Lets you open this wallet in another TON wallet app. Enter your app password to show it.</p>
          <form class="row" on:submit|preventDefault={exportPhrase}>
            <input type="password" placeholder="App password" bind:value={exportPw} />
            <button type="submit" disabled={!exportPw}>Show</button>
          </form>
          {#if exportError}<p class="error small">{exportError}</p>{/if}
        {/if}
      </div>
    </details>
  </div>
</div>

<style>
  .top { align-items: flex-start; gap: 20px; }
  .balances { display: flex; gap: 32px; }
  .big { font-size: 22px; font-weight: 600; }
  .qr { width: 140px; height: 140px; border-radius: var(--radius); }
  .asset { width: 140px; }
  .failed { opacity: .5; }
  summary { cursor: pointer; }
  .words { display: grid; grid-template-columns: repeat(4, 1fr); gap: 4px 16px; margin: 0; padding-left: 24px; }
</style>
