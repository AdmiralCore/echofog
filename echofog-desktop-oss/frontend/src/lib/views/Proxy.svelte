<script>
  import { onMount, onDestroy } from 'svelte';
  import { App, on, errText, copy } from '../api.js';
  import { bytes, price, usdPerGb, microPerMb, when, short } from '../format.js';
  import Icon from '../components/ui/Icon.svelte';
  import SiteCheck from '../components/SiteCheck.svelte';

  let vpn = { running: false, listenAddr: '', relayPeerId: '', bytesRx: 0, bytesTx: 0, activeConns: 0, tunnelReady: false, tunnelNotice: '', wholeComputer: false, wholeComputerSupported: false, wholeComputerError: '' };
  let exits = [];
  let selected = 'auto';
  let pay = { active: false, canPay: false, pricePerGb: '0', remainingGb: 0, needsPayment: false, autoPay: false, chunkUsdt: '1', dailyCapUsdt: '5', spentTodayUsdt: '0', payAsset: 'auto', takesTon: false, chunkTon: '', pending: false, note: '', lastError: '' };
  let payForm = { chunk: '1', cap: '5' };
  let payTouched = false;
  let sharing = { enabled: false, config: { pricePerMb: 0, freeTestKb: 512, uploadKbps: 0 }, accepted: 0, refused: 0, bytes: 0, lastPublish: 0, lastError: '', paymentsReady: false, earnedUsdt: '0' };
  let form = { price: '0', freeKb: 512, mbps: 0 };
  let mode = 'auto'; // 'auto' | 'manual'
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
      ? 'Reachable. This exit asks for payment after its free test traffic.'
      : r.ok
        ? `Reachable: ${r.domainsOk} of ${r.domainsTried} test sites opened, about ${(r.measuredKbps / 1000).toFixed(1)} Mbit/s.`
        : 'The exit did not respond.';
    await loadExits(false);
  });

  const payNow = (amount) => act('pay', async () => {
    await App.PayExit(String(amount));
    notice = 'Payment sent. It is credited once the network confirms it, usually within half a minute.';
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
      });
    }
    await refresh();
  });

  const speed = (e) => e.measuredKbps || e.claimedKbps || 0;
  const mbit = (e) => (speed(e) / 1000).toFixed(speed(e) < 10000 ? 1 : 0) + ' Mbit/s';
  const SORTS = {
    best: (a, b) => (b.score || 0) - (a.score || 0),
    ping: (a, b) => (a.latencyMs || 1e9) - (b.latencyMs || 1e9),
    price: (a, b) => a.pricePerMb - b.pricePerMb,
    speed: (a, b) => speed(b) - speed(a),
  };
  $: sorted = [...exits].sort(SORTS[sortBy]);
  $: best = [...exits].sort(SORTS.best)[0];
  $: if (mode === 'auto') selected = 'auto';
  $: if (mode === 'manual' && selected === 'auto' && sorted.length) selected = sorted[0].peerId;
  $: checkExit = vpn.running ? (vpn.relayPeerId || 'auto') : mode === 'auto' ? 'auto' : selected;
  $: checkLabel = vpn.running ? 'the exit you are connected to' : mode === 'auto' ? 'the exit the automatic choice would pick' : 'exit ' + short(selected);

  const pingClass = (ms) => (!ms ? '' : ms < 80 ? 'good' : ms < 200 ? 'mid' : 'bad');
  const pingText = (ms) => (ms ? Math.round(ms) + ' ms' : 'ping ?');
  const sitesText = (e) => (e.domainsTried ? e.domainsOk + '/' + e.domainsTried + ' sites' : 'not checked');

  async function copyAddr() {
    copied = await copy(vpn.listenAddr);
    setTimeout(() => (copied = false), 1500);
  }

  onMount(async () => {
    refresh();
    act('scan', () => loadExits(true));
    timers = [
      setInterval(() => { refresh(); loadExits(false).catch(() => {}); }, 15000),
      setInterval(() => { if (vpn.running) refreshPay(); }, 5000),
    ];
    for (const t of ['vpn:started', 'vpn:stopped', 'vpn:whole_computer', 'vpn:payment', 'exit:payment', 'exit:session_started', 'exit:session_closed']) {
      offs.push(on(t, refresh));
    }
  });
  onDestroy(() => {
    timers.forEach(clearInterval);
    offs.forEach((off) => off());
  });

  $: connectedExit = exits.find((e) => e.peerId === vpn.relayPeerId);
</script>

<div class="page">
  <div class="stack">
    <h1>Proxy</h1>
    {#if error}<p class="error small">{error}</p>{/if}
    {#if notice}<p class="ok small">{notice}</p>{/if}

    <div class="card stack">
      <div class="row">
        <div class="grow">
          <h2>Use an exit</h2>
          <p class="small muted">
            {#if vpn.running}
              <span class="live-dot" class:ready={vpn.tunnelReady}></span>
              Connected through {connectedExit ? short(connectedExit.peerId) : short(vpn.relayPeerId)}{connectedExit?.latencyMs ? ' · ' + pingText(connectedExit.latencyMs) : ''}.
              {vpn.tunnelReady ? '' : vpn.tunnelNotice || 'Waiting for the tunnel…'}
            {:else}
              Your traffic leaves through another person's computer.
            {/if}
          </p>
        </div>
        <button class="sites-btn" on:click={() => (checkingSites = true)} title="Check which sites open"><Icon name="shield" size={16} /> Check sites</button>
        <button class={vpn.running ? 'danger' : 'primary'} on:click={connect} disabled={busy === 'connect' || (!vpn.running && mode === 'manual' && !selected)}>
          {busy === 'connect' ? 'Working…' : vpn.running ? 'Disconnect' : 'Connect'}
        </button>
      </div>

      {#if vpn.running}
        <div class="row small">
          <span class="muted">SOCKS5 proxy for apps:</span>
          <span class="mono">{vpn.listenAddr}</span>
          <button class="ghost" on:click={copyAddr}>{copied ? 'Copied' : 'Copy'}</button>
          <div class="grow" />
          <span class="muted">received {bytes(vpn.bytesRx)}, sent {bytes(vpn.bytesTx)}</span>
        </div>
        {#if vpn.wholeComputerSupported}
          <label class="row">
            <input type="checkbox" checked={vpn.wholeComputer} disabled={busy === 'whole'} on:change={wholeComputer} />
            <span>Route the whole computer through the exit (asks for administrator rights)</span>
          </label>
          {#if vpn.wholeComputerError}<p class="small error">{vpn.wholeComputerError}</p>{/if}
        {/if}
      {:else}
        <div class="seg" role="tablist">
          <button role="tab" class:active={mode === 'auto'} aria-selected={mode === 'auto'} on:click={() => (mode = 'auto')}>Automatic</button>
          <button role="tab" class:active={mode === 'manual'} aria-selected={mode === 'manual'} on:click={() => (mode = 'manual')}>Choose myself</button>
        </div>

        {#if mode === 'auto'}
          <div class="auto">
            <p class="small muted">The app checks exits in the background and connects to the one that opens your sites fastest for the best price. Pick your sites with <b>Check sites</b>.</p>
            {#if best}
              <div class="exit pick-preview">
                <span class="id mono">{short(best.peerId)}</span>
                <span class="chip ping {pingClass(best.latencyMs)}">{pingText(best.latencyMs)}</span>
                <span class="chip">{best.pricePerMb ? price(best.pricePerMb) : 'Free'}</span>
                <span class="chip">{speed(best) ? mbit(best) : 'speed ?'}</span>
                <span class="chip">{sitesText(best)}</span>
                <span class="grow" />
                <span class="small faint">current best</span>
              </div>
            {:else}
              <p class="small faint">{busy === 'scan' ? 'Looking for exits…' : 'No exits found yet.'}</p>
            {/if}
          </div>
        {:else}
          <div class="row small sortrow">
            <span class="faint">Sort:</span>
            {#each [['best', 'Best'], ['ping', 'Ping'], ['price', 'Price'], ['speed', 'Speed']] as [k, label]}
              <button class="chip-btn" class:on={sortBy === k} on:click={() => (sortBy = k)}>{label}</button>
            {/each}
          </div>
          <div class="exits" role="radiogroup" aria-label="Exits">
            {#each sorted as e (e.peerId)}
              <div class="exit" class:sel={selected === e.peerId} role="radio" aria-checked={selected === e.peerId} tabindex="0"
                on:click={() => (selected = e.peerId)} on:keydown={(ev) => (ev.key === 'Enter' || ev.key === ' ') && (selected = e.peerId)}>
                <span class="radio" />
                <span class="id mono">{short(e.peerId)}</span>
                <span class="chip ping {pingClass(e.latencyMs)}" title="Round trip through the exit">{pingText(e.latencyMs)}</span>
                <span class="chip" title="Price">{e.pricePerMb ? price(e.pricePerMb) : 'Free'}</span>
                <span class="chip" title={e.measuredKbps ? 'Measured' : 'Claimed by the exit'}>{speed(e) ? (e.measuredKbps ? '' : '~') + mbit(e) : 'speed ?'}</span>
                <span class="chip" title="Your sites that opened through this exit at the last check">{sitesText(e)}</span>
                {#if e.paymentRequired}<span class="chip warnchip">needs payment</span>{/if}
                <span class="grow" />
                <button class="ghost small" on:click|stopPropagation={() => probe(e.peerId)} disabled={!!busy}>{busy === 'probe:' + e.peerId ? 'Testing…' : 'Test'}</button>
              </div>
            {:else}
              <p class="small faint">{busy === 'scan' ? 'Looking for exits…' : 'No exits found yet.'}</p>
            {/each}
          </div>
        {/if}
        <div><button class="ghost small" on:click={() => act('scan', () => loadExits(true))} disabled={!!busy}>{busy === 'scan' ? 'Searching…' : 'Search again'}</button></div>
      {/if}
    </div>

    {#if vpn.running && pay.active && Number(pay.pricePerGb) > 0}
      <div class="card stack">
        <h2>Paying this exit</h2>
        <p class="small muted">
          ${pay.pricePerGb} per GB. {pay.remainingGb > 0 ? `${pay.remainingGb.toFixed(2)} GB left.` : 'No paid traffic left.'}
          {pay.pending ? 'A payment is being confirmed.' : ''}
          Spent today: ${pay.spentTodayUsdt}.
        </p>
        {#if pay.note}<p class="note">{pay.note}</p>{/if}
        {#if pay.lastError}<p class="small error">{pay.lastError}</p>{/if}
        <div class="row wrap paywith">
          <span class="small muted">Pay with</span>
          <div class="seg" role="radiogroup" aria-label="Pay with">
            {#each [['auto', 'Automatic'], ['TON', 'TON'], ['USDT', 'USDT']] as [k, label]}
              <button role="radio" aria-checked={pay.payAsset === k} class:active={pay.payAsset === k}
                disabled={busy === 'asset' || (k === 'TON' && !pay.takesTon)}
                title={k === 'auto' ? 'USDT when you have enough of it, otherwise TON' : k === 'TON' && !pay.takesTon ? 'This exit takes USDT only' : ''}
                on:click={() => setAsset(k)}>{label}</button>
            {/each}
          </div>
          {#if pay.chunkTon && pay.payAsset !== 'USDT' && pay.takesTon}<span class="small faint">${payForm.chunk} ≈ {pay.chunkTon} TON now</span>{/if}
        </div>
        <div class="row wrap">
          <label>Top up by, $<input bind:value={payForm.chunk} on:input={() => (payTouched = true)} /></label>
          <label>Daily limit, $<input bind:value={payForm.cap} on:input={() => (payTouched = true)} /></label>
        </div>
        <label class="row">
          <input type="checkbox" checked={pay.autoPay} disabled={busy === 'autopay'} on:change={(e) => saveAutoPay(e.target.checked)} />
          <span>Top up automatically before the traffic runs out, up to the daily limit</span>
        </label>
        <div class="row">
          <button class="primary" on:click={() => payNow(payForm.chunk)} disabled={!pay.canPay || busy === 'pay'}>
            {busy === 'pay' ? 'Sending…' : `Pay $${payForm.chunk} now`}
          </button>
          {#if payTouched}<button on:click={() => saveAutoPay(pay.autoPay)} disabled={!!busy}>Save amounts</button>{/if}
        </div>
      </div>
    {/if}

    <div class="card stack">
      <div class="row">
        <div class="grow">
          <h2>Share your connection</h2>
          <p class="small muted">Others can use your Internet connection as an exit and pay you for the traffic, in TON or USDT.</p>
        </div>
        <button class={sharing.enabled ? 'danger' : 'primary'} on:click={share} disabled={busy === 'share'}>
          {busy === 'share' ? 'Working…' : sharing.enabled ? 'Stop sharing' : 'Start sharing'}
        </button>
      </div>
      {#if sharing.enabled}
        <p class="small">
          Sharing at {price(sharing.config.pricePerMb)}. Published {sharing.lastPublish ? when(sharing.lastPublish) : 'not yet'}.
          {sharing.accepted} sessions, {sharing.refused} refused, {bytes(sharing.bytes)} carried.
          {#if sharing.paymentsReady}Earned ${sharing.earnedUsdt}.{/if}
        </p>
        {#if sharing.lastError}<p class="small error">{sharing.lastError}</p>{/if}
      {:else}
        <div class="row wrap">
          <label>Price, $ per GB (0 = free)<input bind:value={form.price} /></label>
          <label>Free test traffic per client, KB<input type="number" min="0" max="1024" bind:value={form.freeKb} /></label>
          <label>Upload speed, Mbit/s<input type="number" min="0" bind:value={form.mbps} /></label>
          <button class="measure" on:click={measure} disabled={!!busy}>{busy === 'measure' ? 'Measuring…' : 'Measure'}</button>
        </div>
        <p class="note">
          People's traffic will leave from your IP address. Only share a connection you are allowed to share.
          Payments arrive in the wallet of this app, in TON (at the rate when they arrive) or USDT; keep that wallet for traffic only.
        </p>
      {/if}
    </div>

  </div>
</div>

{#if checkingSites}
  <SiteCheck exit={checkExit} exitLabel={checkLabel} on:close={() => { checkingSites = false; loadExits(false).catch(() => {}); }} />
{/if}

<style>
  .row.wrap label { flex: 1; min-width: 160px; }
  .measure { align-self: flex-end; }
  .sites-btn { display: inline-flex; align-items: center; gap: 7px; }
  .paywith { align-items: center; gap: 10px; }
  .seg button:disabled { opacity: .4; }
  .live-dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: var(--warn); margin-right: 4px; vertical-align: middle; }
  .live-dot.ready { background: var(--ok); box-shadow: 0 0 0 3px rgba(62, 207, 142, 0.18); }
  .seg { display: inline-flex; padding: 3px; gap: 3px; border-radius: 12px; background: var(--raised); border: 1px solid var(--line-soft); align-self: flex-start; }
  .seg button { border: none; background: transparent; color: var(--muted); padding: 6px 14px; border-radius: 9px; }
  .seg button:hover:not(:disabled) { background: var(--hover); border: none; }
  .seg button.active, .seg button.active:hover:not(:disabled) { background: var(--accent-soft); color: var(--text); }
  .auto { display: flex; flex-direction: column; gap: 10px; }
  .exits { display: flex; flex-direction: column; gap: 6px; }
  .exit {
    display: flex; align-items: center; gap: 8px; padding: 9px 12px; border-radius: 12px; flex-wrap: wrap;
    background: var(--raised); border: 1px solid var(--line-soft); cursor: pointer; transition: border-color 0.15s, background 0.15s;
  }
  .exit:hover { border-color: var(--line); }
  .exit:focus-visible { outline: none; box-shadow: 0 0 0 3px var(--accent-soft); }
  .exit.sel { border-color: rgba(123, 140, 255, 0.5); background: color-mix(in srgb, var(--accent) 8%, var(--raised)); }
  .pick-preview { cursor: default; }
  .radio { width: 16px; height: 16px; border-radius: 50%; border: 2px solid var(--faint); flex: none; box-sizing: border-box; }
  .exit.sel .radio { border: 5px solid var(--accent); }
  .id { font-size: 12.5px; min-width: 90px; }
  .chip { font-size: 12px; padding: 2px 9px; border-radius: 999px; background: var(--hover); color: var(--muted); font-variant-numeric: tabular-nums; }
  .chip.ping.good { color: var(--ok); background: rgba(62, 207, 142, 0.12); }
  .chip.ping.mid { color: var(--warn); background: rgba(242, 181, 75, 0.12); }
  .chip.ping.bad { color: var(--danger); background: rgba(242, 85, 90, 0.12); }
  .warnchip { color: var(--warn); background: rgba(242, 181, 75, 0.12); }
  .sortrow { gap: 4px; }
  .chip-btn { padding: 3px 10px; font-size: 12px; border-radius: 999px; background: transparent; border-color: var(--line-soft); color: var(--muted); }
  .chip-btn.on { background: var(--accent-soft); color: var(--text); border-color: rgba(123, 140, 255, 0.35); }
</style>
