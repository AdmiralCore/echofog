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
    <div class="page-head">
      <div class="grow">
        <h1>Proxy</h1>
        <p class="muted small">Go online through another person's connection, or sell yours.</p>
      </div>
      <button class="with-icon" on:click={() => (checkingSites = true)} title="Check which sites open"><Icon name="globe" size={16} /> Check sites</button>
    </div>
    {#if error}<p class="error small">{error}</p>{/if}
    {#if notice}<p class="ok small">{notice}</p>{/if}

    <div class="card stack conn" class:running={vpn.running} class:ready={vpn.running && vpn.tunnelReady}>
      <div class="conn-top">
        <button class="power" class:on={vpn.running} on:click={connect}
          disabled={busy === 'connect' || (!vpn.running && mode === 'manual' && !selected)}
          title={vpn.running ? 'Disconnect' : 'Connect'} aria-label={vpn.running ? 'Disconnect' : 'Connect'}>
          {#if busy === 'connect'}<span class="spinner big-spin"></span>{:else}<Icon name="zap" size={26} stroke={2} />{/if}
        </button>
        <div class="grow">
          <h2>{vpn.running ? (vpn.tunnelReady ? 'Connected' : 'Connecting…') : 'Not connected'}</h2>
          <p class="small muted">
            {#if vpn.running}
              Through {connectedExit ? short(connectedExit.peerId) : short(vpn.relayPeerId)}{connectedExit?.latencyMs ? ' · ' + pingText(connectedExit.latencyMs) : ''}.
              {vpn.tunnelReady ? '' : vpn.tunnelNotice || 'Waiting for the tunnel…'}
            {:else}
              Your traffic leaves through another person's computer.
            {/if}
          </p>
        </div>
        <button class={vpn.running ? 'danger' : 'primary'} on:click={connect} disabled={busy === 'connect' || (!vpn.running && mode === 'manual' && !selected)}>
          {busy === 'connect' ? 'Working…' : vpn.running ? 'Disconnect' : 'Connect'}
        </button>
      </div>

      {#if vpn.running}
        <div class="stats">
          <div class="stat"><span class="eyebrow">SOCKS5 for apps</span>
            <span class="row"><span class="mono">{vpn.listenAddr}</span><button class="icon tiny" title="Copy" on:click={copyAddr}><Icon name={copied ? 'check' : 'copy'} size={14} /></button></span></div>
          <div class="stat"><span class="eyebrow">Received</span><span class="num"><Icon name="download" size={14} /> {bytes(vpn.bytesRx)}</span></div>
          <div class="stat"><span class="eyebrow">Sent</span><span class="num"><Icon name="upload" size={14} /> {bytes(vpn.bytesTx)}</span></div>
        </div>
        {#if vpn.wholeComputerSupported}
          <label class="row opt">
            <span class="grow">Route the whole computer through the exit<span class="small muted block">Asks for administrator rights.</span></span>
            <input type="checkbox" class="switch" checked={vpn.wholeComputer} disabled={busy === 'whole'} on:change={wholeComputer} />
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
                <span class="ex-ic"><Icon name="globe" size={16} /></span>
                <span class="id mono">{short(best.peerId)}</span>
                <span class="chip ping {pingClass(best.latencyMs)}">{pingText(best.latencyMs)}</span>
                <span class="chip">{best.pricePerMb ? price(best.pricePerMb) : 'Free'}</span>
                <span class="chip">{speed(best) ? mbit(best) : 'speed ?'}</span>
                <span class="chip">{sitesText(best)}</span>
                <span class="grow" />
                <span class="small faint">current best</span>
              </div>
            {:else}
              <p class="small faint row">{#if busy === 'scan'}<span class="spinner"></span> Looking for exits…{:else}No exits found yet.{/if}</p>
            {/if}
          </div>
        {:else}
          <div class="row small sortrow">
            <span class="faint">Sort</span>
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
                {#if e.paymentRequired}<span class="chip warn">needs payment</span>{/if}
                <span class="grow" />
                <button class="ghost small" on:click|stopPropagation={() => probe(e.peerId)} disabled={!!busy}>{busy === 'probe:' + e.peerId ? 'Testing…' : 'Test'}</button>
              </div>
            {:else}
              <p class="small faint row">{#if busy === 'scan'}<span class="spinner"></span> Looking for exits…{:else}No exits found yet.{/if}</p>
            {/each}
          </div>
        {/if}
        <div><button class="ghost small with-icon" on:click={() => act('scan', () => loadExits(true))} disabled={!!busy}><Icon name="refresh" size={14} /> {busy === 'scan' ? 'Searching…' : 'Search again'}</button></div>
      {/if}
    </div>

    {#if vpn.running && pay.active && Number(pay.pricePerGb) > 0}
      <div class="card stack">
        <div class="card-head">
          <span class="ico"><Icon name="coin" size={18} /></span>
          <div class="grow">
            <h2>Paying this exit</h2>
            <p class="small muted">
              ${pay.pricePerGb} per GB. {pay.remainingGb > 0 ? `${pay.remainingGb.toFixed(2)} GB left.` : 'No paid traffic left.'}
              {pay.pending ? 'A payment is being confirmed.' : ''}
              Spent today: ${pay.spentTodayUsdt}.
            </p>
          </div>
        </div>
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
        <label class="row opt">
          <span class="grow">Top up automatically before the traffic runs out, up to the daily limit</span>
          <input type="checkbox" class="switch" checked={pay.autoPay} disabled={busy === 'autopay'} on:change={(e) => saveAutoPay(e.target.checked)} />
        </label>
        <div class="row">
          <button class="primary" on:click={() => payNow(payForm.chunk)} disabled={!pay.canPay || busy === 'pay'}>
            {busy === 'pay' ? 'Sending…' : `Pay $${payForm.chunk} now`}
          </button>
          {#if payTouched}<button on:click={() => saveAutoPay(pay.autoPay)} disabled={!!busy}>Save amounts</button>{/if}
        </div>
      </div>
    {/if}

    <div class="card stack" class:sharing={sharing.enabled}>
      <div class="card-head">
        <span class="ico" class:ok={sharing.enabled}><Icon name="upload" size={18} /></span>
        <div class="grow">
          <h2>Share your connection {#if sharing.enabled}<span class="chip ok live-chip"><span class="pulse"></span> live</span>{/if}</h2>
          <p class="small muted">Others can use your Internet connection as an exit and pay you for the traffic, in TON or USDT.</p>
        </div>
        <button class={sharing.enabled ? 'danger' : 'primary'} on:click={share} disabled={busy === 'share'}>
          {busy === 'share' ? 'Working…' : sharing.enabled ? 'Stop sharing' : 'Start sharing'}
        </button>
      </div>
      {#if sharing.enabled}
        <div class="stats">
          <div class="stat"><span class="eyebrow">Price</span><span class="num">{price(sharing.config.pricePerMb)}</span></div>
          <div class="stat"><span class="eyebrow">Sessions</span><span class="num">{sharing.accepted} <span class="faint small">/ {sharing.refused} refused</span></span></div>
          <div class="stat"><span class="eyebrow">Carried</span><span class="num">{bytes(sharing.bytes)}</span></div>
          {#if sharing.paymentsReady}<div class="stat"><span class="eyebrow">Earned</span><span class="num ok">${sharing.earnedUsdt}</span></div>{/if}
        </div>
        <p class="small faint">Published {sharing.lastPublish ? when(sharing.lastPublish) : 'not yet'}.</p>
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
  .paywith { align-items: center; gap: 10px; }
  .seg button:disabled { opacity: 0.4; }
  .block { display: block; margin-top: 2px; }
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
  .chip.ping.bad { color: var(--danger); background: var(--danger-soft); }
  .sortrow { gap: 4px; }
  .chip-btn { padding: 4px 12px; font-size: 12px; border-radius: 999px; background: transparent; border-color: var(--line-soft); color: var(--muted); }
  .chip-btn.on, .chip-btn.on:hover:not(:disabled) { background: var(--accent-soft); color: var(--accent); border-color: transparent; }

  .live-chip { margin-left: 6px; vertical-align: middle; font-size: 11px; }
  .pulse { width: 6px; height: 6px; border-radius: 50%; background: currentColor; animation: pulse 1.4s ease-in-out infinite; }
  @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }
</style>
