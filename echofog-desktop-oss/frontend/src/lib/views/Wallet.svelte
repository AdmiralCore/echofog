<script>
  import { onMount, onDestroy } from 'svelte';
  import QRCode from 'qrcode';
  import { App, errText, copy, openURL } from '../api.js';
  import { amount, when, short } from '../format.js';
  import Icon from '../components/ui/Icon.svelte';
  import VaultCard from '../components/VaultCard.svelte';
  import { t, lang } from '../i18n/index.js';

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
    if (!send.to.trim()) sendError = $t('wallet.errNoTo');
    else if (!(n > 0)) sendError = $t('wallet.errAmount');
    else if (balance && n > available) sendError = $t('wallet.errOverBalance');
    else if (send.asset === 'USDT' && balance && Number(balance.ton) < 0.065) sendError = $t('wallet.errNeedTon');
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
    <div class="page-head">
      <div class="grow">
        <h1>{$t('wallet.title')} {#if testnet}<span class="chip warn">testnet</span>{/if}</h1>
        <p class="muted small">{$t('wallet.subtitle')}</p>
      </div>
    </div>

    {#if netError}<p class="error small">{netError}</p>{/if}

    <div class="hero">
      <div class="hero-main">
        <div class="eyebrow light">{$t('wallet.balance')}</div>
        <div class="balances">
          <div class="bal">
            <span class="big">{balance ? amount(balance.usdt || 0, $lang) : '…'}</span><span class="cur">USDT</span>
          </div>
          <div class="bal second">
            <span class="mid">{balance ? amount(balance.ton, $lang) : '…'}</span><span class="cur">TON</span>
            {#if balance?.usdRate}<span class="usd">≈ ${(Number(balance.ton) * balance.usdRate).toFixed(2)}</span>{/if}
          </div>
        </div>
        <div class="addr">
          <span class="mono">{wallet?.address || '…'}</span>
        </div>
        <div class="row wrap">
          <button class="glass with-icon" on:click={copyAddress} disabled={!wallet}><Icon name={copied ? 'check' : 'copy'} size={15} />{copied ? $t('common.copied') : $t('wallet.copyAddress')}</button>
          {#if onRamp}<button class="glass with-icon" on:click={() => openURL(onRamp)}><Icon name="coin" size={15} /> {$t('wallet.buyCard')}</button>{/if}
        </div>
      </div>
      {#if qr}<img class="qr" src={qr} alt={$t('wallet.qrAlt')} />{/if}
    </div>

    <p class="note">
      {$t('wallet.note')}
      {$t('wallet.noteFee')}
      {#if !onRamp}{$t('wallet.noteGetUsdt')}{/if}
    </p>

    <VaultCard {explorer} balanceTon={balance ? balance.ton : null} on:changed={refresh} />

    <div class="card stack">
      <div class="card-head">
        <span class="ico"><Icon name="send" size={18} /></span>
        <div class="grow"><h2>{$t('wallet.send')}</h2><p class="small muted">{$t('wallet.sendHint')}</p></div>
      </div>
      {#key step}
      <div class="stack step">
      {#if step === 'form'}
        <div class="seg asset-seg">
          {#each ['USDT', 'TON'] as a}
            <button class:active={send.asset === a} on:click={() => (send.asset = a)}>{a}</button>
          {/each}
        </div>
        <label>{$t('wallet.recipient')}<input bind:value={send.to} spellcheck="false" placeholder="UQ…" /></label>
        <div class="row wrap">
          <label class="amt">{$t('wallet.amount')}
            <span class="amt-in"><input bind:value={send.amount} placeholder="0.00" inputmode="decimal" /><button class="ghost small max" on:click={() => (send.amount = send.asset === 'TON' ? Math.max(0, available - 0.05).toFixed(4) : String(available))} disabled={!balance}>{$t('wallet.max')}</button></span>
          </label>
          <label class="grow">{$t('wallet.comment')}<input bind:value={send.comment} /></label>
        </div>
        <div><button class="primary" on:click={review}>{$t('wallet.review')}</button></div>
      {:else if step === 'review' || step === 'sending'}
        <div class="review">
          <div class="r-amt">{send.amount} <span>{send.asset}</span></div>
          <div class="small muted">{$t('wallet.to')}</div>
          <div class="mono">{send.to}</div>
          {#if send.comment}<div class="small muted">“{send.comment}”</div>{/if}
        </div>
        <div class="row">
          <button class="primary" on:click={confirmSend} disabled={step === 'sending'}>{#if step === 'sending'}<span class="spinner"></span> {$t('wallet.sending')}{:else}{$t('wallet.confirmSend')}{/if}</button>
          <button on:click={() => (step = 'form')} disabled={step === 'sending'}>{$t('common.back')}</button>
        </div>
      {:else}
        <div class="done">
          <span class="done-ic"><Icon name="check" size={26} stroke={2.6} /></span>
          <div>
            <p class="ok"><b>{$t('wallet.sentBold')}</b> {$t('wallet.sentAfter')}</p>
            <p class="small"><a href={`${explorer}/transaction/${sentHash}`} on:click|preventDefault={() => openURL(`${explorer}/transaction/${sentHash}`)}>{$t('wallet.viewExplorer')}</a></p>
          </div>
        </div>
        <div><button on:click={() => (step = 'form')}>{$t('wallet.newTransfer')}</button></div>
      {/if}
      </div>
      {/key}
      {#if sendError}<p class="error small">{sendError}</p>{/if}
    </div>

    <div class="card stack">
      <div class="card-head">
        <span class="ico"><Icon name="refresh" size={18} /></span>
        <div class="grow"><h2>{$t('wallet.history')}</h2></div>
      </div>
      {#if history.length}
        <div class="txs">
          {#each history as tx (tx.hash)}
            <div class="tx" class:failed={tx.failed}>
              <span class="tx-ic" class:in={tx.incoming}><Icon name={tx.incoming ? 'download' : 'upload'} size={16} /></span>
              <div class="grow tx-body">
                <div class="row">
                  <span class="grow mono cp">{short(tx.counterparty)}</span>
                  <span class="tx-amt" class:in={tx.incoming}>{tx.incoming ? '+' : '−'}{amount(tx.ton, $lang)} {tx.asset || 'TON'}</span>
                </div>
                <div class="row small">
                  <span class="grow faint ellip">{tx.comment || (tx.incoming ? $t('wallet.received') : $t('wallet.sent'))}{tx.failed ? ' · ' + $t('wallet.failed') : ''}</span>
                  <span class="faint">{when(tx.time, $lang)}</span>
                </div>
              </div>
            </div>
          {/each}
        </div>
      {:else}
        <p class="small muted">{$t('wallet.noTx')}</p>
      {/if}
    </div>

    {#if support.available}
      <div class="card stack">
        <div class="card-head">
          <span class="ico ok"><Icon name="zap" size={18} /></span>
          <div class="grow">
            <h2>{$t('wallet.support')}</h2>
            <p class="small muted">{$t('wallet.supportHint')}</p>
          </div>
        </div>
        <div class="seg">
          {#each [0, 5, 10] as p}
            <button class:active={support.percent === p} on:click={() => setShare(p)}>{p === 0 ? $t('wallet.off') : p + '%'}</button>
          {/each}
        </div>
        {#if Number(support.pendingUsdt) > 0 || Number(support.pendingTon) > 0}
          <p class="small faint">{$t('wallet.pending', { usdt: support.pendingUsdt, ton: support.pendingTon })}</p>
        {/if}
      </div>
    {/if}

    <details class="card">
      <summary class="muted">{$t('wallet.phrase')}</summary>
      <div class="stack" style="margin-top: 14px">
        {#if words.length}
          <ol class="words">{#each words as w, i}<li><span class="n">{i + 1}</span><span class="mono">{w}</span></li>{/each}</ol>
          <div><button on:click={() => (words = [])}>{$t('common.hide')}</button></div>
        {:else}
          <p class="small muted">{$t('wallet.phraseHint')}</p>
          <form class="row" on:submit|preventDefault={exportPhrase}>
            <input type="password" placeholder={$t('wallet.appPassword')} bind:value={exportPw} />
            <button type="submit" disabled={!exportPw}>{$t('common.show')}</button>
          </form>
          {#if exportError}<p class="error small">{exportError}</p>{/if}
        {/if}
      </div>
    </details>
  </div>
</div>

<style>
  .hero {
    position: relative; overflow: hidden; display: flex; gap: 22px; align-items: center; flex-wrap: wrap;
    padding: 24px; border-radius: var(--radius-xl); color: #fff;
    background:
      radial-gradient(420px 260px at 100% 0%, rgba(255, 255, 255, 0.18), transparent 60%),
      linear-gradient(135deg, var(--accent), var(--accent-2));
    box-shadow: var(--glow), var(--shadow);
  }
  .hero::after {
    content: ''; position: absolute; width: 260px; height: 260px; right: -80px; bottom: -140px; border-radius: 50%;
    border: 40px solid rgba(255, 255, 255, 0.07); pointer-events: none;
  }
  .hero-main { flex: 1; min-width: 240px; display: flex; flex-direction: column; gap: 12px; position: relative; z-index: 1; }
  .eyebrow.light { color: rgba(255, 255, 255, 0.7); }
  .balances { display: flex; flex-direction: column; gap: 2px; }
  .bal { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
  .big { font-size: clamp(30px, 5vw, 40px); font-weight: 750; letter-spacing: -0.03em; line-height: 1.1; font-variant-numeric: tabular-nums; }
  .mid { font-size: 20px; font-weight: 650; font-variant-numeric: tabular-nums; }
  .cur { font-weight: 600; opacity: 0.8; }
  .usd { font-size: 13px; opacity: 0.7; }
  .addr { padding: 8px 12px; border-radius: 12px; background: rgba(0, 0, 0, 0.16); max-width: 520px; }
  .addr .mono { color: rgba(255, 255, 255, 0.92); }
  button.glass { background: rgba(255, 255, 255, 0.16); border-color: rgba(255, 255, 255, 0.2); color: #fff; backdrop-filter: blur(6px); }
  button.glass:hover:not(:disabled) { background: rgba(255, 255, 255, 0.26); border-color: rgba(255, 255, 255, 0.3); }
  .qr { width: 136px; height: 136px; border-radius: 16px; padding: 8px; background: #fff; position: relative; z-index: 1; box-shadow: 0 10px 30px rgba(0, 0, 0, 0.25); }

  .step { animation: fade-up 0.3s var(--ease); }
  .asset-seg { min-width: 200px; }
  .amt { width: 200px; }
  .amt-in { position: relative; display: block; margin-top: 6px; }
  .amt-in input { margin: 0; padding-right: 56px; font-variant-numeric: tabular-nums; }
  .max { position: absolute; right: 4px; top: 50%; transform: translateY(-50%); padding: 3px 9px; font-size: 11.5px; color: var(--accent); }
  .max:active:not(:disabled) { transform: translateY(-50%) scale(0.95); }
  .review { padding: 16px; border-radius: 16px; background: var(--raised); display: flex; flex-direction: column; gap: 4px; }
  .r-amt { font-size: 26px; font-weight: 720; letter-spacing: -0.02em; }
  .r-amt span { font-size: 15px; color: var(--muted); font-weight: 600; }
  .done { display: flex; align-items: center; gap: 14px; }
  .done-ic { width: 48px; height: 48px; border-radius: 50%; display: grid; place-items: center; background: var(--ok-soft); color: var(--ok); animation: pop 0.45s var(--spring); }

  .txs { display: flex; flex-direction: column; gap: 2px; margin: 0 -8px; }
  .tx { display: flex; align-items: center; gap: 12px; padding: 9px 8px; border-radius: 12px; transition: background var(--dur); }
  .tx:hover { background: var(--hover); }
  .tx.failed { opacity: 0.5; }
  .tx-ic { width: 36px; height: 36px; border-radius: 12px; flex: none; display: grid; place-items: center; background: var(--raised); color: var(--muted); }
  .tx-ic.in { background: var(--ok-soft); color: var(--ok); }
  .tx-body { display: flex; flex-direction: column; gap: 1px; }
  .cp { font-size: 12.5px; word-break: normal; }
  .tx-amt { font-weight: 650; font-variant-numeric: tabular-nums; }
  .tx-amt.in { color: var(--ok); }
  .ellip { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  .words { display: grid; grid-template-columns: repeat(auto-fill, minmax(120px, 1fr)); gap: 6px; margin: 0; padding: 0; list-style: none; }
  .words li { display: flex; gap: 8px; align-items: center; padding: 6px 10px; border-radius: 10px; background: var(--raised); }
  .words .n { color: var(--faint); font-size: 11px; width: 16px; text-align: right; }
  .words .mono { word-break: normal; }
</style>

