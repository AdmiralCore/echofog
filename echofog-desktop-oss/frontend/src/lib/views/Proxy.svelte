<script>
  import { onMount, onDestroy } from 'svelte';
  import { App, on, errText, copy } from '../api.js';
  import { bytes, price, usdPerGb, microPerMb, when, short, amount } from '../format.js';
  import Icon from '../components/ui/Icon.svelte';
  import SiteCheck from '../components/SiteCheck.svelte';
  import PeerList from '../components/PeerList.svelte';
  import RelayCard from '../components/RelayCard.svelte';
  import { parseRelayLink } from '../contact.js';
  import { t, lang } from '../i18n/index.js';
  import { me } from '../stores/session.js';

  let vpn = { running: false, listenAddr: '', relayPeerId: '', bytesRx: 0, bytesTx: 0, activeConns: 0, tunnelReady: false, tunnelNotice: '', exitState: '', refused: 0, lastRefusalReason: '', lastRefusalHost: '', lastRefusalAt: 0, wholeComputer: false, wholeComputerSupported: false, wholeComputerError: '' };
  let exits = [];
  let selected = 'auto';
  let pay = { active: false, canPay: false, pricePerGb: '0', remainingGb: 0, needsPayment: false, autoPay: false, chunkUsdt: '1', dailyCapUsdt: '5', spentTodayUsdt: '0', payAsset: 'auto', takesTon: false, chunkTon: '', pending: false, note: '', lastError: '', paysByVault: false };
  let payForm = { chunk: '1', cap: '5' };
  let payTouched = false;
  let sharing = { enabled: false, config: { pricePerMb: 0, freeTestKb: 512, uploadKbps: 0, freePeers: [] }, accepted: 0, refused: 0, bytes: 0, lastPublish: 0, lastError: '', paymentsReady: false, earnedUsdt: '0' };
  let vouchers = { owedTon: '0', cashedTon: '0', quoteNano: 0 };
  let form = { price: '0', freeKb: 512, mbps: 0 };
  let mode = 'auto'; // 'auto' | 'manual' | 'direct'
  let direct = ''; // 'direct': a relay link, host:port/<peer id> or a peer ID
  let contacts = [];
  let sortBy = 'best'; // 'best' | 'ping' | 'price' | 'speed'
  let checkingSites = false;

  let busy = '';
  let error = '';
  let notice = '';
  let copied = false;
  const offs = [];
  let timers = [];

  async function act(name, fn) {
    busy = name;
    error = notice = '';
    try {
      await fn();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }

  async function refresh() {
    try {
      vpn = await App.GetVPNStatus();
      sharing = await App.GetSharingStatus();
      try { vouchers = await App.GetVoucherEarnings(); } catch (_) { /* older core */ }
      if (!sharing.enabled && sharing.config?.pricePerMb && form.price === '0') {
        form = { ...form, price: usdPerGb(sharing.config.pricePerMb).toFixed(3), freeKb: sharing.config.freeTestKb };
      }
      await refreshPay();
    } catch (err) {
      error = errText(err);
    }
  }

  async function refreshPay() {
    try {
      pay = await App.GetExitPayment();
      if (!payTouched) payForm = { chunk: pay.chunkUsdt || '1', cap: pay.dailyCapUsdt || '5' };
    } catch (_) { /* not connected */ }
  }

  async function loadExits(rescan) {
    if (rescan) await App.RefreshExitNodes();
    exits = (await App.ListExitNodes()) || [];
  }

  const connect = () => act('connect', async () => {
    if (vpn.running) await App.StopVPN();
    else if (mode === 'direct') await App.StartVPNVia(direct.trim());
    else await App.StartVPN(selected);
    await refresh();
  });

  const wholeComputer = () => act('whole', async () => {
    await App.SetWholeComputer(!vpn.wholeComputer);
    await refresh();
  });

  const probe = (id) => act('probe:' + id, async () => {
    const r = await App.ProbeExitNode(id);
    notice = r.paymentRequired
      ? $t('proxy.probe.paid')
      : r.ok
        ? $t('proxy.probe.ok', { ok: r.domainsOk, n: r.domainsTried, mbps: (r.measuredKbps / 1000).toFixed(1) })
        : $t('proxy.probe.fail');
    await loadExits(false);
  });

  const payNow = (amount) => act('pay', async () => {
    await App.PayExit(String(amount));
    notice = $t('proxy.pay.sent');
    await refreshPay();
  });

  const setAsset = (asset) => act('asset', async () => {
    await App.SetPayAsset(asset);
    await refreshPay();
  });

  const saveAutoPay = (enabled) => act('autopay', async () => {
    await App.SetAutoPay(enabled, String(payForm.chunk), String(payForm.cap));
    payTouched = false;
    await refreshPay();
  });

  const measure = () => act('measure', async () => {
    const kbps = await App.MeasureUploadCapacity();
    form = { ...form, mbps: Math.round(kbps / 100) / 10 };
  });

  const share = () => act('share', async () => {
    if (sharing.enabled) {
      await App.StopSharing();
    } else {
      await App.StartSharing({
        pricePerMb: microPerMb(form.price),
        freeTestKb: Math.min(1024, Math.max(0, Math.round(Number(form.freeKb) || 0))),
        uploadKbps: Math.round((Number(form.mbps) || 0) * 1000),
        freePeers: sharing.config.freePeers || [],
      });
    }
    await refresh();
  });

  const setFreePeers = (ids) => act('free', async () => {
    await App.SetExitFreePeers(ids);
    await refresh();
  });

  const speed = (e) => e.measuredKbps || e.claimedKbps || 0;
  $: mbit = (e) => (speed(e) / 1000).toFixed(speed(e) < 10000 ? 1 : 0) + ' ' + $t('unit.mbps');
  const SORTS = {
    best: (a, b) => (b.score || 0) - (a.score || 0),
    ping: (a, b) => (a.latencyMs || 1e9) - (b.latencyMs || 1e9),
    price: (a, b) => a.pricePerMb - b.pricePerMb,
    speed: (a, b) => speed(b) - speed(a),
  };
  $: sorted = [...exits].sort(SORTS[sortBy]);
  $: best = [...exits].sort(SORTS.best)[0];
  $: if (mode !== 'manual') selected = 'auto';
  $: directId = parseRelayLink(direct)?.peerId || (/^[0-9a-f]{64}$/i.test(direct.trim()) ? direct.trim().toLowerCase() : '');
  $: blocked = !vpn.running && ((mode === 'manual' && !selected) || (mode === 'direct' && !directId));
  $: if (mode === 'manual' && selected === 'auto' && sorted.length) selected = sorted[0].peerId;
  $: checkExit = vpn.running ? (vpn.relayPeerId || 'auto') : mode === 'auto' ? 'auto' : mode === 'direct' ? directId || 'auto' : selected;
  $: checkLabel = vpn.running ? $t('proxy.check.connected') : mode === 'auto' || (mode === 'direct' && !directId) ? $t('proxy.check.auto') : $t('proxy.check.exit', { id: short(checkExit) });

  const pingClass = (ms) => (!ms ? '' : ms < 80 ? 'good' : ms < 200 ? 'mid' : 'bad');
  $: pingText = (ms) => (ms ? $t('proxy.pingMs', { n: Math.round(ms) }) : $t('proxy.pingUnknown'));
  $: sitesText = (e) => (e.domainsTried ? $t('proxy.sitesOk', { ok: e.domainsOk, n: e.domainsTried }) : $t('proxy.notChecked'));

  async function copyAddr() {
    copied = await copy(vpn.listenAddr);
    setTimeout(() => (copied = false), 1500);
  }

  onMount(async () => {
    refresh();
    App.GetContacts().then((c) => (contacts = c || [])).catch(() => {});
    act('scan', () => loadExits(true));
    timers = [
      setInterval(() => { refresh(); loadExits(false).catch(() => {}); }, 15000),
      setInterval(() => { if (vpn.running) refreshPay(); }, 5000),
    ];
    for (const t of ['vpn:started', 'vpn:stopped', 'vpn:exit_state', 'vpn:whole_computer', 'vpn:payment', 'exit:payment', 'exit:session_started', 'exit:session_closed']) {
      offs.push(on(t, refresh));
    }
  });
  onDestroy(() => {
    timers.forEach(clearInterval);
    offs.forEach((off) => off());
  });

  $: connectedExit = exits.find((e) => e.peerId === vpn.relayPeerId);
  // the peer link being up is not enough: the exit itself says whether it serves us
  $: exitProblem = vpn.running && ['disabled', 'quota', 'unreachable'].includes(vpn.exitState) ? vpn.exitState : '';
  $: refusalText = vpn.running && vpn.refused > 0 && vpn.lastRefusalReason
    ? $t('proxy.refused', { n: vpn.refused, reason: $t('proxy.refusal.' + vpn.lastRefusalReason), host: vpn.lastRefusalHost, when: when(vpn.lastRefusalAt, $lang) })
    : '';
</script>

<div class="page">
  <div class="stack">
    <div class="page-head">
      <div class="grow">
        <h1>{$t('proxy.title')}</h1>
        <p class="muted small">{$t('proxy.subtitle')}</p>
      </div>
      <button class="with-icon" on:click={() => (checkingSites = true)} title={$t('proxy.checkSitesTitle')}><Icon name="globe" size={16} /> {$t('proxy.checkSites')}</button>
    </div>
    {#if error}<p class="error small">{error}</p>{/if}
    {#if notice}<p class="ok small">{notice}</p>{/if}

    <div class="card stack conn" class:running={vpn.running} class:ready={vpn.running && vpn.tunnelReady} class:problem={!!exitProblem}>
      <div class="conn-top">
        <button class="power" class:on={vpn.running} on:click={connect}
          disabled={busy === 'connect' || blocked}
          title={vpn.running ? $t('proxy.disconnect') : $t('proxy.connect')} aria-label={vpn.running ? $t('proxy.disconnect') : $t('proxy.connect')}>
          {#if busy === 'connect'}<span class="spinner big-spin"></span>{:else}<Icon name="zap" size={26} stroke={2} />{/if}
        </button>
        <div class="grow">
          <h2>{vpn.running ? (vpn.tunnelReady ? $t('proxy.connected') : exitProblem ? $t('proxy.notServing') : $t('proxy.connecting')) : $t('proxy.notConnected')}</h2>
          <p class="small muted">
            {#if vpn.running}
              {$t('proxy.through', { id: connectedExit ? short(connectedExit.peerId) : short(vpn.relayPeerId) })}{connectedExit?.latencyMs ? ' · ' + pingText(connectedExit.latencyMs) : ''}.
              {vpn.tunnelReady || exitProblem ? '' : vpn.exitState === 'checking' ? $t('proxy.exit.checking') : vpn.tunnelNotice || $t('proxy.waitingTunnel')}
            {:else}
              {$t('proxy.idleHint')}
            {/if}
          </p>
        </div>
        <button class={vpn.running ? 'danger' : 'primary'} on:click={connect} disabled={busy === 'connect' || blocked}>
          {busy === 'connect' ? $t('common.working') : vpn.running ? $t('proxy.disconnect') : $t('proxy.connect')}
        </button>
      </div>

      {#if exitProblem}
        <div class="small exit-warn" role="alert">
          <Icon name="shield" size={14} />
          <span>{$t('proxy.exit.' + exitProblem)}
            {#if exitProblem === 'quota' && $me?.id}<span class="block">{$t('proxy.exit.yourId')} <span class="mono">{$me.id}</span>
              <button class="icon tiny" title={$t('common.copy')} on:click={() => copy($me.id)}><Icon name="copy" size={12} /></button></span>{/if}
          </span>
        </div>
      {/if}
      {#if refusalText}<p class="small faint">{refusalText}</p>{/if}
      {#if vpn.running}
        <div class="stats">
          <div class="stat"><span class="eyebrow">{$t('proxy.socks')}</span>
            <span class="row"><span class="mono">{vpn.listenAddr}</span><button class="icon tiny" title={$t('common.copy')} on:click={copyAddr}><Icon name={copied ? 'check' : 'copy'} size={14} /></button></span></div>
          <div class="stat"><span class="eyebrow">{$t('proxy.received')}</span><span class="num"><Icon name="download" size={14} /> {bytes(vpn.bytesRx, $lang)}</span></div>
          <div class="stat"><span class="eyebrow">{$t('proxy.sent')}</span><span class="num"><Icon name="upload" size={14} /> {bytes(vpn.bytesTx, $lang)}</span></div>
        </div>
        {#if vpn.wholeComputerSupported}
          <label class="row opt">
            <span class="grow">{$t('proxy.whole')}<span class="small muted block">{$t('proxy.wholeAdmin')}</span></span>
            <input type="checkbox" class="switch" checked={vpn.wholeComputer} disabled={busy === 'whole'} on:change={wholeComputer} />
          </label>
          {#if vpn.wholeComputer}
            <p class="small leak-warn"><Icon name="shield" size={14} /> {$t('proxy.wholeLeakWarning')}</p>
          {/if}
          {#if vpn.wholeComputerError}<p class="small error">{vpn.wholeComputerError}</p>{/if}
        {/if}
      {:else}
        <div class="seg" role="tablist">
          <button role="tab" class:active={mode === 'auto'} aria-selected={mode === 'auto'} on:click={() => (mode = 'auto')}>{$t('proxy.mode.auto')}</button>
          <button role="tab" class:active={mode === 'manual'} aria-selected={mode === 'manual'} on:click={() => (mode = 'manual')}>{$t('proxy.mode.manual')}</button>
          <button role="tab" class:active={mode === 'direct'} aria-selected={mode === 'direct'} on:click={() => (mode = 'direct')}>{$t('proxy.mode.direct')}</button>
        </div>

        {#if mode === 'auto'}
          <div class="auto">
            <p class="small muted">{$t('proxy.autoBefore')}<b>{$t('proxy.checkSites')}</b>{$t('proxy.autoAfter')}</p>
            {#if best}
              <div class="exit pick-preview">
                <span class="ex-ic"><Icon name="globe" size={16} /></span>
                <span class="id mono">{short(best.peerId)}</span>
                <span class="chip ping {pingClass(best.latencyMs)}">{pingText(best.latencyMs)}</span>
                <span class="chip">{best.pricePerMb ? price(best.pricePerMb, $lang) : $t('common.Free')}</span>
                <span class="chip">{speed(best) ? mbit(best) : $t('proxy.speedUnknown')}</span>
                <span class="chip">{sitesText(best)}</span>
                <span class="grow" />
                <span class="small faint">{$t('proxy.currentBest')}</span>
              </div>
            {:else}
              <p class="small faint row">{#if busy === 'scan'}<span class="spinner"></span> {$t('proxy.looking')}{:else}{$t('proxy.noExits')}{/if}</p>
            {/if}
          </div>
        {:else if mode === 'direct'}
          <div class="auto">
            <p class="small muted">{$t('proxy.directHint')}</p>
            <input bind:value={direct} placeholder="echofog://relay/host:port/peer-id" on:keydown={(e) => e.key === 'Enter' && !blocked && connect()} />
            {#if direct.trim() && !directId}<p class="small error">{$t('proxy.directBad')}</p>{/if}
          </div>
        {:else}
          <div class="row small sortrow">
            <span class="faint">{$t('proxy.sort')}</span>
            {#each ['best', 'ping', 'price', 'speed'] as k}
              <button class="chip-btn" class:on={sortBy === k} on:click={() => (sortBy = k)}>{$t('proxy.sort.' + k)}</button>
            {/each}
          </div>
          <div class="exits" role="radiogroup" aria-label={$t('proxy.exits')}>
            {#each sorted as e (e.peerId)}
              <div class="exit" class:sel={selected === e.peerId} role="radio" aria-checked={selected === e.peerId} tabindex="0"
                on:click={() => (selected = e.peerId)} on:keydown={(ev) => (ev.key === 'Enter' || ev.key === ' ') && (selected = e.peerId)}>
                <span class="radio" />
                <span class="id mono">{short(e.peerId)}</span>
                <span class="chip ping {pingClass(e.latencyMs)}" title={$t('proxy.pingTitle')}>{pingText(e.latencyMs)}</span>
                <span class="chip" title={$t('proxy.priceTitle')}>{e.pricePerMb ? price(e.pricePerMb, $lang) : $t('common.Free')}</span>
                <span class="chip" title={e.measuredKbps ? $t('proxy.measured') : $t('proxy.claimed')}>{speed(e) ? (e.measuredKbps ? '' : '~') + mbit(e) : $t('proxy.speedUnknown')}</span>
                <span class="chip" title={$t('proxy.sitesTitle')}>{sitesText(e)}</span>
                {#if e.paymentRequired}<span class="chip warn">{$t('proxy.needsPayment')}</span>{/if}
                <span class="grow" />
                <button class="ghost small" on:click|stopPropagation={() => probe(e.peerId)} disabled={!!busy}>{busy === 'probe:' + e.peerId ? $t('proxy.testing') : $t('proxy.test')}</button>
              </div>
            {:else}
              <p class="small faint row">{#if busy === 'scan'}<span class="spinner"></span> {$t('proxy.looking')}{:else}{$t('proxy.noExits')}{/if}</p>
            {/each}
          </div>
        {/if}
        {#if mode !== 'direct'}<div><button class="ghost small with-icon" on:click={() => act('scan', () => loadExits(true))} disabled={!!busy}><Icon name="refresh" size={14} /> {busy === 'scan' ? $t('proxy.searching') : $t('proxy.searchAgain')}</button></div>{/if}
      {/if}
    </div>

    {#if vpn.running && pay.active && Number(pay.pricePerGb) > 0}
      <div class="card stack">
        <div class="card-head">
          <span class="ico"><Icon name="coin" size={18} /></span>
          <div class="grow">
            <h2>{$t('proxy.pay.title')} {#if pay.paysByVault}<span class="chip ok live-chip">{$t('proxy.pay.fromDeposit')}</span>{/if}</h2>
            <p class="small muted">
              {$t('proxy.pay.perGb', { price: pay.pricePerGb })} {pay.remainingGb > 0 ? $t('proxy.pay.left', { gb: pay.remainingGb.toFixed(2) }) : $t('proxy.pay.noneLeft')}
              {pay.pending ? $t('proxy.pay.pending') : ''}
              {$t('proxy.pay.spent', { amount: pay.spentTodayUsdt })}
              {#if pay.paysByVault}{$t('proxy.pay.vaultNote')}{/if}
            </p>
          </div>
        </div>
        {#if pay.note}<p class="note">{pay.note}</p>{/if}
        {#if pay.lastError}<p class="small error">{pay.lastError}</p>{/if}
        <div class="row wrap paywith">
          <span class="small muted">{$t('proxy.pay.with')}</span>
          <div class="seg" role="radiogroup" aria-label={$t('proxy.pay.with')}>
            {#each [['auto', $t('proxy.pay.auto')], ['TON', 'TON'], ['USDT', 'USDT']] as [k, label]}
              <button role="radio" aria-checked={pay.payAsset === k} class:active={pay.payAsset === k}
                disabled={busy === 'asset' || (k === 'TON' && !pay.takesTon)}
                title={k === 'auto' ? $t('proxy.pay.autoTitle') : k === 'TON' && !pay.takesTon ? $t('proxy.pay.usdtOnly') : ''}
                on:click={() => setAsset(k)}>{label}</button>
            {/each}
          </div>
          {#if pay.chunkTon && pay.payAsset !== 'USDT' && pay.takesTon}<span class="small faint">{$t('proxy.pay.tonNow', { usd: payForm.chunk, ton: pay.chunkTon })}</span>{/if}
        </div>
        <div class="row wrap">
          <label>{$t('proxy.pay.chunk')}<input bind:value={payForm.chunk} on:input={() => (payTouched = true)} /></label>
          <label>{$t('proxy.pay.cap')}<input bind:value={payForm.cap} on:input={() => (payTouched = true)} /></label>
        </div>
        <label class="row opt">
          <span class="grow">{$t('proxy.pay.autoPay')}</span>
          <input type="checkbox" class="switch" checked={pay.autoPay} disabled={busy === 'autopay'} on:change={(e) => saveAutoPay(e.target.checked)} />
        </label>
        <div class="row">
          <button class="primary" on:click={() => payNow(payForm.chunk)} disabled={!pay.canPay || busy === 'pay'}>
            {busy === 'pay' ? $t('proxy.pay.sending') : $t('proxy.pay.now', { amount: payForm.chunk })}
          </button>
          {#if payTouched}<button on:click={() => saveAutoPay(pay.autoPay)} disabled={!!busy}>{$t('proxy.pay.save')}</button>{/if}
        </div>
      </div>
    {/if}

    <div class="card stack" class:sharing={sharing.enabled}>
      <div class="card-head">
        <span class="ico" class:ok={sharing.enabled}><Icon name="upload" size={18} /></span>
        <div class="grow">
          <h2>{$t('proxy.share.title')} {#if sharing.enabled}<span class="chip ok live-chip"><span class="pulse"></span> {$t('proxy.share.live')}</span>{/if}</h2>
          <p class="small muted">{$t('proxy.share.hint')}</p>
        </div>
        <button class={sharing.enabled ? 'danger' : 'primary'} on:click={share} disabled={busy === 'share'}>
          {busy === 'share' ? $t('common.working') : sharing.enabled ? $t('proxy.share.stop') : $t('proxy.share.start')}
        </button>
      </div>
      {#if sharing.enabled}
        <div class="stats">
          <div class="stat"><span class="eyebrow">{$t('proxy.share.price')}</span><span class="num">{price(sharing.config.pricePerMb, $lang)}</span></div>
          <div class="stat"><span class="eyebrow">{$t('proxy.share.sessions')}</span><span class="num">{sharing.accepted} <span class="faint small">{$t('proxy.share.refused', { n: sharing.refused })}</span></span></div>
          <div class="stat"><span class="eyebrow">{$t('proxy.share.carried')}</span><span class="num">{bytes(sharing.bytes, $lang)}</span></div>
          {#if sharing.paymentsReady}<div class="stat"><span class="eyebrow">{$t('proxy.share.earned')}</span><span class="num ok">${sharing.earnedUsdt}</span></div>{/if}
        </div>
        <p class="small faint">{$t('proxy.share.published', { when: sharing.lastPublish ? when(sharing.lastPublish, $lang) : $t('proxy.share.notYet') })}</p>
        {#if sharing.lastError}<p class="small error">{sharing.lastError}</p>{/if}
      {/if}
      {#if (sharing.enabled && sharing.paymentsReady) || Number(vouchers.owedTon) > 0 || Number(vouchers.cashedTon) > 0}
        <div class="sub">
          <div class="eyebrow">{$t('proxy.share.deposits')}</div>
          <div class="stats">
            <div class="stat"><span class="eyebrow">{$t('proxy.share.owed')}</span><span class="num">{amount(vouchers.owedTon, $lang)} TON</span></div>
            <div class="stat"><span class="eyebrow">{$t('proxy.share.collected')}</span><span class="num ok">{amount(vouchers.cashedTon, $lang)} TON</span></div>
          </div>
          <p class="small muted">{$t('proxy.share.depositNote')}</p>
        </div>
      {/if}
      {#if !sharing.enabled}
        <div class="row wrap">
          <label>{$t('proxy.share.priceLabel')}<input bind:value={form.price} /></label>
          <label>{$t('proxy.share.freeKb')}<input type="number" min="0" max="1024" bind:value={form.freeKb} /></label>
          <label>{$t('proxy.share.upload')}<input type="number" min="0" bind:value={form.mbps} /></label>
          <button class="measure" on:click={measure} disabled={!!busy}>{busy === 'measure' ? $t('proxy.share.measuring') : $t('proxy.share.measure')}</button>
        </div>
        <p class="note">
          {$t('proxy.share.legal')}
          {$t('proxy.share.wallet')}
        </p>
      {/if}
      <div class="sub">
        <div class="eyebrow">{$t('proxy.share.freeFor')}</div>
        <p class="small muted">{$t('proxy.share.freeHint')}</p>
        <PeerList ids={sharing.config.freePeers || []} {contacts} disabled={busy === 'free'} label={$t('proxy.share.freeFor')}
          empty={$t('proxy.share.freeEmpty')} on:change={(e) => setFreePeers(e.detail)} />
      </div>
    </div>

    <RelayCard {contacts} />
  </div>
</div>

{#if checkingSites}
  <SiteCheck exit={checkExit} exitLabel={checkLabel} on:close={() => { checkingSites = false; loadExits(false).catch(() => {}); }} />
{/if}

<style>
  .row.wrap label { flex: 1; min-width: 160px; }
  .measure { align-self: flex-end; }
  .paywith { align-items: center; gap: 10px; }
  .seg button:disabled { opacity: 0.4; }
  .block { display: block; margin-top: 2px; }
  .sub { display: flex; flex-direction: column; gap: 8px; }
  .opt { align-items: center; }
  .icon.tiny { width: 24px; height: 24px; border-radius: 7px; }

  .conn { position: relative; overflow: hidden; }
  .conn::before {
    content: ''; position: absolute; inset: 0; pointer-events: none; opacity: 0; transition: opacity 0.6s var(--ease);
    background: radial-gradient(500px 200px at 0% 0%, var(--ok-soft), transparent 70%);
  }
  .conn.ready::before { opacity: 1; }
  .conn > * { position: relative; }
  .conn-top { display: flex; align-items: center; gap: 16px; flex-wrap: wrap; }
  .power {
    width: 64px; height: 64px; border-radius: 50%; padding: 0; flex: none; display: grid; place-items: center;
    background: var(--raised); color: var(--muted); border: 1px solid var(--line);
  }
  .power:hover:not(:disabled) { color: var(--accent); border-color: var(--accent); background: var(--accent-soft); transform: scale(1.05); }
  .power.on { background: linear-gradient(135deg, var(--ok), color-mix(in srgb, var(--ok) 60%, var(--accent))); color: #fff; border-color: transparent; box-shadow: 0 0 0 6px var(--ok-soft), 0 8px 24px var(--ok-soft); }
  .conn.running.problem .power.on { background: var(--danger); box-shadow: 0 0 0 6px var(--danger-soft); animation: none; }
  .running:not(.ready) .power.on { animation: breathe 1.6s ease-in-out infinite; }
  .power.on:hover:not(:disabled) { color: #fff; filter: brightness(1.08); }
  @keyframes breathe { 0%, 100% { box-shadow: 0 0 0 4px var(--ok-soft); } 50% { box-shadow: 0 0 0 12px transparent; } }
  .big-spin { width: 22px; height: 22px; }

  .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 10px; }
  .stat { display: flex; flex-direction: column; gap: 4px; padding: 12px 14px; border-radius: 14px; background: var(--raised); min-width: 0; }
  .num { display: inline-flex; align-items: center; gap: 6px; font-weight: 650; font-size: 15px; font-variant-numeric: tabular-nums; }
  .num.ok { color: var(--ok); }

  .auto { display: flex; flex-direction: column; gap: 10px; }
  .exits { display: flex; flex-direction: column; gap: 6px; }
  .exit {
    display: flex; align-items: center; gap: 8px; padding: 10px 12px; border-radius: 14px; flex-wrap: wrap;
    background: var(--raised); border: 1.5px solid transparent; cursor: pointer;
    transition: border-color var(--dur), background var(--dur), transform 0.3s var(--spring);
    animation: fade-up 0.3s var(--ease) both;
  }
  .exit:hover { border-color: var(--line); transform: translateX(2px); }
  .exit:focus-visible { outline: none; box-shadow: 0 0 0 3px var(--accent-soft); }
  .exit.sel { border-color: var(--accent); background: var(--accent-soft); }
  .pick-preview { cursor: default; }
  .pick-preview:hover { transform: none; border-color: transparent; }
  .ex-ic { width: 28px; height: 28px; border-radius: 9px; display: grid; place-items: center; background: var(--accent-soft); color: var(--accent); }
  .radio { width: 18px; height: 18px; border-radius: 50%; border: 2px solid var(--faint); flex: none; transition: border 0.3s var(--spring); }
  .exit.sel .radio { border: 5.5px solid var(--accent); }
  .id { font-size: 12.5px; min-width: 90px; word-break: normal; }
  .chip.ping.good { color: var(--ok); background: var(--ok-soft); }
  .chip.ping.mid { color: var(--warn); background: var(--warn-soft); }
  .leak-warn { display: flex; gap: 6px; align-items: flex-start; color: var(--warn); background: var(--warn-soft); border-radius: 10px; padding: 8px 10px; line-height: 1.4; margin-top: 2px; }
  .leak-warn :global(svg) { flex: none; margin-top: 2px; }
  .exit-warn { display: flex; gap: 6px; align-items: flex-start; color: var(--danger); background: var(--danger-soft); border-radius: 10px; padding: 8px 10px; line-height: 1.4; }
  .exit-warn :global(svg) { flex: none; margin-top: 2px; }
  .exit-warn .mono { word-break: break-all; }
  .chip.ping.bad { color: var(--danger); background: var(--danger-soft); }
  .sortrow { gap: 4px; }
  .chip-btn { padding: 4px 12px; font-size: 12px; border-radius: 999px; background: transparent; border-color: var(--line-soft); color: var(--muted); }
  .chip-btn.on, .chip-btn.on:hover:not(:disabled) { background: var(--accent-soft); color: var(--accent); border-color: transparent; }

  .live-chip { margin-left: 6px; vertical-align: middle; font-size: 11px; }
  .pulse { width: 6px; height: 6px; border-radius: 50%; background: currentColor; animation: pulse 1.4s ease-in-out infinite; }
  @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }
</style>
