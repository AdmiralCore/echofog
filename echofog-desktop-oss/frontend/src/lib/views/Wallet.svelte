<script>
  import { onMount, onDestroy } from 'svelte';
  import QRCode from 'qrcode';
  import { App, errText, copy, openURL } from '../api.js';
  import { amount, when, short } from '../format.js';
  import './skin.css';
  import Button from '../components/ui/Button.svelte';
  import IconButton from '../components/ui/IconButton.svelte';
  import Input from '../components/ui/Input.svelte';
  import Tabs from '../components/ui/Tabs.svelte';
  import Badge from '../components/ui/Badge.svelte';
  import Icon from '../components/ui/Icon.svelte';
  import VaultCard from '../components/VaultCard.svelte';
  import TestnetNotice from '../components/TestnetNotice.svelte';
  import { pay } from '../stores/paygate.js';
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
      sentHash = await pay(() => fn(send.to.trim(), String(send.amount).replace(',', '.'), send.comment.trim()));
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

<div class="page skin">
  <div class="wl">
    <header class="head">
      <div class="grow">
        <h1>{$t('wallet.title')} {#if testnet}<Badge variant="warn">testnet</Badge>{/if}</h1>
        <p class="muted small">{$t('wallet.subtitle')}</p>
      </div>
    </header>

    <TestnetNotice />
    {#if netError}<p class="notice err small" role="alert"><Icon name="error" size={15} /> {netError}</p>{/if}

    <p class="notice quiet small" role="note"><Icon name="info" size={15} /> <span>{$t('wallet.note')} {$t('wallet.noteFee')} {#if !onRamp}{$t('wallet.noteGetUsdt')}{/if}</span></p>

    <div class="cols">
      <div class="col">
        <section class="balance">
          <div class="b-main">
            <div class="eyebrow">{$t('wallet.balance')}</div>
            <div class="bal">
              <span class="big">{balance ? amount(balance.usdt || 0, $lang) : '…'}</span><span class="cur">USDT</span>
            </div>
            <div class="bal second">
              <span class="mid">{balance ? amount(balance.ton, $lang) : '…'}</span><span class="cur">TON</span>
              {#if balance?.usdRate}<span class="usd">≈ ${(Number(balance.ton) * balance.usdRate).toFixed(2)}</span>{/if}
            </div>
            <div class="addr"><span class="mono">{wallet?.address || '…'}</span></div>
            <div class="row wrap">
              <Button size="sm" icon={copied ? 'check' : 'copy'} on:click={copyAddress} disabled={!wallet}>{copied ? $t('common.copied') : $t('wallet.copyAddress')}</Button>
              {#if onRamp}<Button size="sm" icon="coin" on:click={() => openURL(onRamp)}>{$t('wallet.buyCard')}</Button>{/if}
            </div>
          </div>
          {#if qr}<img class="qr" src={qr} alt={$t('wallet.qrAlt')} />{/if}
        </section>

        <VaultCard {explorer} balanceTon={balance ? balance.ton : null} on:changed={refresh} />

        <section class="card stack">
          <div class="card-head">
            <span class="ico"><Icon name="send" size={18} /></span>
            <div class="grow"><h2>{$t('wallet.send')}</h2><p class="small muted">{$t('wallet.sendHint')}</p></div>
          </div>
          {#key step}
          <div class="stack step">
          {#if step === 'form'}
            <Tabs variant="segment" label={$t('wallet.send')} value={send.asset} on:change={(e) => (send.asset = e.detail)} items={[{ id: 'USDT', label: 'USDT' }, { id: 'TON', label: 'TON' }]} />
            <Input label={$t('wallet.recipient')} bind:value={send.to} spellcheck="false" placeholder="UQ…" />
            <div class="row wrap amt-row">
              <div class="amt">
                <Input label={$t('wallet.amount')} bind:value={send.amount} placeholder="0.00" inputmode="decimal">
                  <Button slot="end" variant="ghost" size="sm" className="max" on:click={() => (send.amount = send.asset === 'TON' ? Math.max(0, available - 0.05).toFixed(4) : String(available))} disabled={!balance}>{$t('wallet.max')}</Button>
                </Input>
              </div>
              <div class="grow"><Input label={$t('wallet.comment')} bind:value={send.comment} /></div>
            </div>
            <div><Button variant="primary" on:click={review}>{$t('wallet.review')}</Button></div>
          {:else if step === 'review' || step === 'sending'}
            <div class="review">
              <div class="r-amt">{send.amount} <span>{send.asset}</span></div>
              <div class="small muted">{$t('wallet.to')}</div>
              <div class="mono">{send.to}</div>
              {#if send.comment}<div class="small muted">“{send.comment}”</div>{/if}
            </div>
            <div class="row">
              <Button variant="primary" loading={step === 'sending'} on:click={confirmSend}>{step === 'sending' ? $t('wallet.sending') : $t('wallet.confirmSend')}</Button>
              <Button on:click={() => (step = 'form')} disabled={step === 'sending'}>{$t('common.back')}</Button>
            </div>
          {:else}
            <div class="done">
              <span class="done-ic"><Icon name="check" size={22} stroke={2.2} /></span>
              <div>
                <p class="ok"><b>{$t('wallet.sentBold')}</b> {$t('wallet.sentAfter')}</p>
                <p class="small"><a href={`${explorer}/transaction/${sentHash}`} on:click|preventDefault={() => openURL(`${explorer}/transaction/${sentHash}`)}>{$t('wallet.viewExplorer')}</a></p>
              </div>
            </div>
            <div><Button on:click={() => (step = 'form')}>{$t('wallet.newTransfer')}</Button></div>
          {/if}
          </div>
          {/key}
          {#if sendError}<p class="error small" role="alert">{sendError}</p>{/if}
        </section>
      </div>

      <div class="col">
        <section class="card stack">
          <div class="card-head">
            <span class="ico"><Icon name="clock" size={18} /></span>
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
        </section>

        {#if support.available}
          <section class="card stack">
            <div class="card-head">
              <span class="ico ok"><Icon name="zap" size={18} /></span>
              <div class="grow">
                <h2>{$t('wallet.support')}</h2>
                <p class="small muted">{$t('wallet.supportHint')}</p>
              </div>
            </div>
            <Tabs variant="segment" label={$t('wallet.support')} value={support.percent} on:change={(e) => setShare(e.detail)}
              items={[0, 5, 10].map((p) => ({ id: p, label: p === 0 ? $t('wallet.off') : p + '%' }))} />
            {#if Number(support.pendingUsdt) > 0 || Number(support.pendingTon) > 0}
              <p class="small faint">{$t('wallet.pending', { usdt: support.pendingUsdt, ton: support.pendingTon })}</p>
            {/if}
          </section>
        {/if}

        <details class="card">
          <summary class="muted">{$t('wallet.phrase')}</summary>
          <div class="stack" style="margin-top: 14px">
            {#if words.length}
              <ol class="words">{#each words as w, i}<li><span class="n">{i + 1}</span><span class="mono">{w}</span></li>{/each}</ol>
              <div><Button on:click={() => (words = [])}>{$t('common.hide')}</Button></div>
            {:else}
              <p class="small muted">{$t('wallet.phraseHint')}</p>
              <form class="row end" on:submit|preventDefault={exportPhrase}>
                <div class="grow"><Input type="password" placeholder={$t('wallet.appPassword')} aria-label={$t('wallet.appPassword')} bind:value={exportPw} /></div>
                <Button type="submit" disabled={!exportPw}>{$t('common.show')}</Button>
              </form>
              {#if exportError}<p class="error small" role="alert">{exportError}</p>{/if}
            {/if}
          </div>
        </details>
      </div>
    </div>
  </div>
</div>

<style>
  .page { height: 100%; overflow-y: auto; padding: var(--sp-6) var(--sp-6) var(--sp-12); }
  .wl { max-width: 1040px; margin: 0 auto; display: flex; flex-direction: column; gap: var(--sp-4); }
  .head { display: flex; align-items: flex-end; gap: var(--sp-3); }
  .head h1 { display: flex; align-items: center; gap: var(--sp-2); }
  .cols { display: grid; grid-template-columns: minmax(0, 1.25fr) minmax(0, 1fr); gap: var(--sp-4); align-items: start; }
  .col { display: flex; flex-direction: column; gap: var(--sp-4); min-width: 0; }
  @media (max-width: 880px) { .cols { grid-template-columns: minmax(0, 1fr); } }

  .notice { display: flex; align-items: flex-start; gap: var(--sp-2); padding: var(--sp-2) var(--sp-3); border-radius: var(--r-md); line-height: 1.45; }
  .notice :global(svg) { flex: none; margin-top: 2px; }
  .notice.quiet { background: var(--surface-2); color: var(--muted); }
  .notice.err { background: var(--danger-soft); color: var(--danger-fg); }

  .balance { display: flex; gap: var(--sp-5); align-items: center; flex-wrap: wrap; padding: var(--sp-5); border-radius: var(--r-xl); background: var(--surface-1); border: 1px solid var(--border); box-shadow: var(--shadow-sm); }
  .b-main { flex: 1; min-width: 240px; display: flex; flex-direction: column; gap: var(--sp-2); }
  .bal { display: flex; align-items: baseline; gap: var(--sp-2); flex-wrap: wrap; }
  .big { font-size: 32px; line-height: 1.1; font-weight: var(--fw-semibold); letter-spacing: -0.02em; font-variant-numeric: tabular-nums; }
  .mid { font-size: var(--fs-lg); font-weight: var(--fw-semibold); font-variant-numeric: tabular-nums; }
  .cur { font-weight: var(--fw-medium); color: var(--muted); }
  .usd { font-size: var(--fs-sm); color: var(--faint); }
  .addr { padding: 6px 10px; border-radius: var(--r-md); background: var(--surface-2); }
  .qr { width: 120px; height: 120px; border-radius: var(--r-lg); padding: 6px; background: #fff; flex: none; }

  .step { animation: fade-up 0.2s var(--ease); }
  .amt { width: 200px; }
  .amt-row { align-items: flex-start; }
  :global(.max) { color: var(--accent-fg); }
  .review { padding: var(--sp-4); border-radius: var(--r-lg); background: var(--surface-2); display: flex; flex-direction: column; gap: 4px; }
  .r-amt { font-size: 24px; font-weight: var(--fw-semibold); letter-spacing: -0.02em; }
  .r-amt span { font-size: var(--fs-md); color: var(--muted); font-weight: var(--fw-medium); }
  .done { display: flex; align-items: center; gap: var(--sp-3); }
  .done-ic { width: 40px; height: 40px; border-radius: 50%; display: grid; place-items: center; background: var(--ok-soft); color: var(--ok); }
  .end { align-items: flex-end; }

  .txs { display: flex; flex-direction: column; margin: 0 -8px; }
  .tx { display: flex; align-items: center; gap: var(--sp-3); padding: 8px; border-radius: var(--r-md); }
  .tx:hover { background: var(--hover); }
  .tx.failed { opacity: 0.5; }
  .tx-ic { width: 32px; height: 32px; border-radius: var(--r-md); flex: none; display: grid; place-items: center; background: var(--surface-2); color: var(--muted); }
  .tx-ic.in { background: var(--ok-soft); color: var(--ok); }
  .tx-body { display: flex; flex-direction: column; gap: 1px; }
  .cp { font-size: var(--fs-sm); word-break: normal; }
  .tx-amt { font-weight: var(--fw-semibold); font-variant-numeric: tabular-nums; font-size: var(--fs-sm); }
  .tx-amt.in { color: var(--ok-fg); }
  .ellip { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  .words { display: grid; grid-template-columns: repeat(auto-fill, minmax(112px, 1fr)); gap: var(--sp-1); margin: 0; padding: 0; list-style: none; }
  .words li { display: flex; gap: var(--sp-2); align-items: center; padding: 6px 10px; border-radius: var(--r-sm); background: var(--surface-2); }
  .words .n { color: var(--faint); font-size: var(--fs-2xs); width: 16px; text-align: right; }
  .words .mono { word-break: normal; }
</style>
