<script>
  import { onMount, onDestroy } from 'svelte';
  import { App, on, errText, copy } from '../api.js';
  import { EXIT_PROBLEMS, isSiteFailure, refusalKey, historyKey } from '../vpnreason.js';
  import { bytes, price, usdPerGb, microPerMb, when, short, amount, duration, countryName } from '../format.js';
  import Icon from '../components/ui/Icon.svelte';
  import './skin.css';
  import Button from '../components/ui/Button.svelte';
  import IconButton from '../components/ui/IconButton.svelte';
  import Input from '../components/ui/Input.svelte';
  import Tabs from '../components/ui/Tabs.svelte';
  import Toggle from '../components/ui/Toggle.svelte';
  import Checkbox from '../components/ui/Checkbox.svelte';
  import SiteCheck from '../components/SiteCheck.svelte';
  import PeerList from '../components/PeerList.svelte';
  import RelayCard from '../components/RelayCard.svelte';
  import AccessCard from '../components/AccessCard.svelte';
  import TestnetNotice from '../components/TestnetNotice.svelte';
  import ExitAccessCard from '../components/ExitAccessCard.svelte';
  import { parseRelayLink } from '../contact.js';
  import { t, lang } from '../i18n/index.js';
  import { ask } from '../stores/dialog.js';
  import { me } from '../stores/session.js';
  import { view } from '../stores/nav.js';

  let vpn = { running: false, listenAddr: '', relayPeerId: '', bytesRx: 0, bytesTx: 0, activeConns: 0, tunnelReady: false, tunnelNotice: '', exitState: '', refused: 0, lastRefusalReason: '', lastRefusalHost: '', lastRefusalAt: 0, autoSwitch: true, searching: false, switches: 0, lastSwitch: { from: '', to: '', reason: '', at: 0 }, wholeComputer: false, wholeComputerSupported: false, wholeComputerError: '' };
  let exits = [];
  let selected = 'auto';
  let pay = { active: false, canPay: false, pricePerGb: '0', remainingGb: 0, needsPayment: false, autoPay: false, chunkUsdt: '1', dailyCapUsdt: '5', spentTodayUsdt: '0', payAsset: 'auto', takesTon: false, chunkTon: '', pending: false, note: '', lastError: '', paysByVault: false };
  let payForm = { chunk: '1', cap: '5' };
  let payTouched = false;
  let sharing = { enabled: false, config: { pricePerMb: 0, freeTestKb: 512, uploadKbps: 0, freePeers: [] }, accepted: 0, refused: 0, bytes: 0, lastPublish: 0, lastError: '', paymentsReady: false, earnedUsdt: '0' };
  let vouchers = { owedTon: '0', cashedTon: '0', quoteNano: 0 };
  let form = { price: '0', freeKb: 512, mbps: 0 };
  let tab = 'connect'; // 'connect' | 'share'
  let mode = 'auto'; // 'auto' | 'manual' | 'direct'
  let direct = ''; // 'direct': a relay link, host:port/<peer id> or a peer ID
  let contacts = [];
  let sortBy = 'best'; // 'best' | 'ping' | 'price' | 'speed'
  let checkingSites = false;
  let history = [];
  let saved = [];
  let openExit = '';
  let renaming = '';
  let nameDraft = '';
  let showAllHistory = false;
  let copiedLink = '';
  let accessMode = 'public'; // a hidden exit is not published: people use it by its link

  let busy = '';
  let error = '';
  let notice = '';
  let copied = false;
  const offs = [];
  let timers = [];

  // the core's words for a server across the border with no cover site (by the list or by a link)
  const NO_COVER = /no cover site|across the border over noise/;

  async function act(name, fn) {
    busy = name;
    error = notice = '';
    try {
      await fn();
    } catch (err) {
      const msg = errText(err);
      error = NO_COVER.test(msg) ? $t('proxy.noCover') : msg;
    } finally {
      busy = '';
    }
  }

  async function refresh() {
    try {
      vpn = await App.GetVPNStatus();
      checkHeld();
      sharing = await App.GetSharingStatus();
      try { accessMode = (await App.GetAccessStatus()).mode; } catch (_) { /* older core */ }
      try { vouchers = await App.GetVoucherEarnings(); } catch (_) { /* older core */ }
      if (!sharing.enabled && sharing.config?.pricePerMb && form.price === '0') {
        form = { ...form, price: usdPerGb(sharing.config.pricePerMb).toFixed(3), freeKb: sharing.config.freeTestKb };
      }
      await refreshPay();
      await loadHistory();
    } catch (err) {
      error = errText(err);
    }
  }

  async function loadHistory() {
    try {
      history = (await App.GetVPNHistory()) || [];
      saved = (await App.GetSavedExits()) || [];
    } catch (_) { /* older core */ }
  }

  // the exit's share link: what used to be copied around by hand
  const exitLink = (s) => (s.addr ? 'echofog://relay/' + s.addr + '/' + s.peerId : s.peerId);

  const connectLast = () => act('connect', async () => {
    await App.ConnectLastExit();
    await refresh();
  });

  const connectTo = (s) => act('connect', async () => {
    if (vpn.running) await App.StopVPNForSwitch(); // the kill switch keeps the computer blocked meanwhile
    await App.StartVPNVia(exitLink(s));
    await refresh();
  });

  const clearHistory = async () => {
    if (!(await ask({ message: $t('proxy.hist.clearConfirm'), confirm: $t('proxy.hist.clear'), danger: true }))) return;
    act('clear', async () => {
      await App.ClearVPNHistory();
      await loadHistory();
    });
  };

  const toggleStar = (e) => act('star', async () => {
    await App.SetExitFavorite(e.peerId, !e.favorite);
    await loadHistory();
  });

  function startRename(e) {
    renaming = e.peerId;
    nameDraft = e.name || '';
  }

  const saveName = (e) => act('rename', async () => {
    await App.SetExitName(e.peerId, nameDraft);
    renaming = '';
    await loadHistory();
  });

  const forget = async (e) => {
    if (!(await ask({ message: $t('proxy.hist.forgetConfirm'), confirm: $t('common.delete'), danger: true }))) return;
    act('forget', async () => {
      await App.ForgetExit(e.peerId);
      await loadHistory();
    });
  };

  async function copyLink(s) {
    if (await copy(exitLink(s))) {
      copiedLink = s.peerId + (s.startedAt || '');
      setTimeout(() => (copiedLink = ''), 1500);
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

  const setKillSwitch = (on) => act('kill', async () => {
    await App.SetKillSwitch(on);
    await refresh();
  });

  // the kill switch blocks the computer's traffic: no exit serves us, or EchoFog went away earlier
  let held = false;
  const checkHeld = async () => { try { held = await App.KillSwitchHeld(); } catch (_) { held = false; } };
  const unblock = () => act('unblock', async () => {
    if (vpn.wholeComputer) await App.SetWholeComputer(false);
    else await App.ReleaseKillSwitch();
    await checkHeld();
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
    } else if (sharing.access) {
      // protocol 2: the exit is free; say whose address the traffic leaves with, then turn it on
      if (!(await ask({ title: $t('proxy.share.title'), message: $t('proxy.share.ipWarning'), confirm: $t('proxy.share.start') }))) return;
      await App.StartSharing({ pricePerMb: 0, freeTestKb: 0, uploadKbps: 0, freePeers: [] });
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

  const setAutoSwitch = (on) => act('autoswitch', async () => {
    await App.SetVPNAutoSwitch(on);
    await refresh();
  });

  const topUpDeposit = () => view.set('wallet');

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
  // an invitation link (echofog://join?...) is read by the core: it knows the network and the format
  let invite = null;
  let inviteErr = '';
  $: isInvite = /^echofog:\/\/join\?/i.test(direct.trim());
  $: readInvite(isInvite ? direct.trim() : '');
  async function readInvite(text) {
    invite = null;
    inviteErr = '';
    if (!text) return;
    try {
      const info = await App.ParseInvite(text);
      if (text === direct.trim()) invite = info;
    } catch (err) {
      const msg = errText(err);
      const net = /network "([^"]*)"/.exec(msg);
      inviteErr = /newer version/.test(msg) ? $t('access.tooNew') : net ? $t('access.otherNet', { net: net[1] }) : msg;
    }
  }
  $: directId = isInvite ? invite?.peerId || '' : parseRelayLink(direct)?.peerId || (/^[0-9a-f]{64}$/i.test(direct.trim()) ? direct.trim().toLowerCase() : '');
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
    for (const t of ['vpn:started', 'vpn:stopped', 'vpn:exit_state', 'vpn:searching', 'vpn:switched', 'vpn:whole_computer', 'vpn:payment', 'exit:payment', 'exit:session_started', 'exit:session_closed', 'vpn:exits_located']) {
      offs.push(on(t, refresh));
    }
  });
  onDestroy(() => {
    timers.forEach(clearInterval);
    offs.forEach((off) => off());
  });

  $: connectedExit = exits.find((e) => e.peerId === vpn.relayPeerId);
  $: lastExit = history.find((s) => !s.active);
  $: shownExits = showAllHistory ? saved : saved.slice(0, 6);
  $: exitName = (e) => e.name || contactName(e.peerId) || short(e.peerId);
  // where an exit is, when its location is known (it is looked up for the exits in the history):
  // "Netherlands, Amsterdam · 5f09…"
  $: whereExit = (e) => {
    const geo = e.countryCode ? e : saved.find((s) => s.peerId === e.peerId);
    return (geo?.countryCode ? countryName(geo.countryCode) + (geo.city ? ', ' + geo.city : '') + ' · ' : '') + short(e.peerId);
  };
  $: sessionsOf = (id) => history.filter((s) => s.peerId === id);
  $: priceText = (e) => (e.pricePerMb ? price(e.pricePerMb, $lang) : e.priceKnown ? $t('common.Free') : $t('proxy.hist.priceUnknown'));
  // what is left on the exit for us, as it last said; it stays there after we leave
  $: leftText = (x) => (x.remaining > 0 ? $t('proxy.hist.left', { left: bytes(x.remaining, $lang) }) : $t('proxy.hist.leftNone'));
  $: leftTitle = (x) => $t('proxy.hist.leftTitle', { when: when(x.remainingAt, $lang) });
  // streams that did not open: "dial failed" is the site (not reachable from the exit, often a
  // name with no IPv4), the rest point at the exit or the way to it
  $: refusals = (x) => {
    const by = x.refusedBy || {};
    const site = Object.entries(by).reduce((n, [k, v]) => (isSiteFailure(k) ? n + v : n), 0);
    const exit = Object.entries(by).reduce((n, [k, v]) => (isSiteFailure(k) ? n : n + v), 0);
    const known = site + exit > 0;
    const title = Object.entries(by).sort((a, b) => b[1] - a[1])
      .map(([k, v]) => `${$t(historyKey(k))}: ${v}`).join('\n');
    return { site, exit, known, title: (title ? title + '\n\n' : '') + $t('proxy.hist.refusedHelp') };
  };
  $: contactName = (id) => contacts.find((c) => (c.peer_id || '').toLowerCase() === id)?.alias || '';
  const sessionSeconds = (s) => Math.max(0, (s.active ? Math.floor(Date.now() / 1000) : s.endedAt || s.updatedAt) - s.startedAt);
  // the peer link being up is not enough: the exit itself says whether it serves us
  // the exit about to be used, to warn before connecting when it is paid
  $: target = mode === 'manual' ? exits.find((e) => e.peerId === selected) : mode === 'auto' ? best : null;
  $: switchedRecently = vpn.running && vpn.lastSwitch?.at && Date.now() / 1000 - vpn.lastSwitch.at < 120;
  $: exitProblem = vpn.running && EXIT_PROBLEMS.has(vpn.exitState) ? vpn.exitState : '';
  $: refusalText = vpn.running && vpn.refused > 0 && vpn.lastRefusalReason
    ? $t('proxy.refused', { n: vpn.refused, reason: $t(refusalKey(vpn.lastRefusalReason)), host: vpn.lastRefusalHost, when: when(vpn.lastRefusalAt, $lang) })
    : '';
</script>

<div class="page skin px">
  <div class="pxwrap">
    <header class="head">
      <div class="grow">
        <h1>{$t('proxy.title')}</h1>
        <p class="muted small">{$t('proxy.subtitle')}</p>
      </div>
      <Button icon="globe" on:click={() => (checkingSites = true)} title={$t('proxy.checkSitesTitle')}>{$t('proxy.checkSites')}</Button>
    </header>
    {#if error}<p class="error small" role="alert">{error}</p>{/if}
    {#if notice}<p class="ok small" role="status">{notice}</p>{/if}

    {#if vpn.blocked || (held && !vpn.wholeComputer)}
      <div class="banner warn" role="alert">
        <Icon name="lock" size={16} />
        <span class="grow small">{$t(vpn.blocked ? 'proxy.killBlocked' : 'proxy.killHeld')}</span>
        <Button variant="primary" on:click={unblock} disabled={!!busy}>{$t('proxy.killUnblock')}</Button>
      </div>
    {/if}

    <!-- state of the connection: always on top -->
    <section class="card stack conn" class:running={vpn.running} class:ready={vpn.running && vpn.tunnelReady} class:problem={!!exitProblem} aria-live="polite">
      <div class="conn-top">
        <span class="state-ic" aria-hidden="true">
          {#if busy === 'connect'}<span class="spinner"></span>{:else}<Icon name="zap" size={20} stroke={2} />{/if}
        </span>
        <div class="grow">
          <h2>{vpn.running ? (vpn.tunnelReady ? $t('proxy.connected') : exitProblem ? $t('proxy.notServing') : $t('proxy.connecting')) : $t('proxy.notConnected')}</h2>
          <p class="small muted">
            {#if vpn.running}
              {$t('proxy.through', { id: whereExit(connectedExit || { peerId: vpn.relayPeerId }) })}{connectedExit?.latencyMs ? ' · ' + pingText(connectedExit.latencyMs) : ''}.
              {vpn.tunnelReady || exitProblem ? '' : vpn.exitState === 'checking' ? $t('proxy.exit.checking') : vpn.tunnelNotice || $t('proxy.waitingTunnel')}
            {:else}
              {$t('proxy.idleHint')}
            {/if}
          </p>
        </div>
        <Button variant={vpn.running ? 'danger' : 'primary'} size="lg" on:click={connect} loading={busy === 'connect'} disabled={blocked}
          title={vpn.running ? $t('proxy.disconnect') : $t('proxy.connect')}>
          {busy === 'connect' ? $t('common.working') : vpn.running ? $t('proxy.disconnect') : $t('proxy.connect')}
        </Button>
      </div>

      {#if vpn.running && vpn.searching}
        <p class="small notice warn row"><span class="spinner"></span> {$t('proxy.searchingExit')}</p>
      {:else if switchedRecently}
        <p class="small ok">{$t('proxy.switched', { to: short(vpn.lastSwitch.to), reason: $t('proxy.switchReason.' + vpn.lastSwitch.reason) })}</p>
      {/if}
      {#if exitProblem && !vpn.searching}
        <div class="small notice danger" role="alert">
          <Icon name="shield" size={14} />
          <span>{$t('proxy.exit.' + exitProblem)}
            {#if exitProblem === 'quota' && $me?.id}<span class="block">{$t('proxy.exit.yourId')} <span class="mono">{$me.id}</span>
              <IconButton icon="copy" size="sm" label={$t('common.copy')} on:click={() => copy($me.id)} /></span>{/if}
          </span>
        </div>
      {/if}
      {#if refusalText}<p class="small faint">{refusalText}</p>{/if}

      {#if vpn.running}
        <div class="stats">
          <div class="stat"><span class="eyebrow">{$t('proxy.socks')}</span>
            <span class="row"><span class="mono">{vpn.listenAddr}</span><IconButton icon={copied ? 'check' : 'copy'} size="sm" label={$t('common.copy')} on:click={copyAddr} /></span></div>
          <div class="stat"><span class="eyebrow">{$t('proxy.received')}</span><span class="num"><Icon name="download" size={14} /> {bytes(vpn.bytesRx, $lang)}</span></div>
          <div class="stat"><span class="eyebrow">{$t('proxy.sent')}</span><span class="num"><Icon name="upload" size={14} /> {bytes(vpn.bytesTx, $lang)}</span></div>
        </div>
      {:else if lastExit}
        <div class="row last-exit">
          <Button size="sm" icon="zap" on:click={connectLast} disabled={!!busy}>{$t('proxy.last.connect')}</Button>
          <span class="small faint">{$t('proxy.last.hint', { id: exitName(saved.find((e) => e.peerId === lastExit.peerId) || lastExit), when: when(lastExit.endedAt || lastExit.startedAt, $lang) })}</span>
        </div>
      {/if}

      <div class="opts">
        {#if vpn.running && vpn.wholeComputerSupported}
          <Toggle label={$t('proxy.whole')} hint={$t('proxy.wholeAdmin')} checked={vpn.wholeComputer} disabled={busy === 'whole'} on:change={wholeComputer} />
          <Toggle label={$t('proxy.killSwitch')} hint={$t('proxy.killSwitchHint')} checked={vpn.killSwitch} disabled={busy === 'kill'} on:change={(e) => setKillSwitch(e.detail)} />
          {#if vpn.wholeComputer}
            <p class="small notice warn"><Icon name="shield" size={14} /> {$t(vpn.killSwitch ? 'proxy.killSwitchLimits' : 'proxy.wholeLeakWarning')}</p>
          {/if}
          {#if vpn.wholeComputerError}<p class="small error">{vpn.wholeComputerError}</p>{/if}
        {/if}
        <Toggle label={$t('proxy.autoSwitch')} hint={$t('proxy.autoSwitchHint')} checked={vpn.autoSwitch} disabled={busy === 'autoswitch'} on:change={(e) => setAutoSwitch(e.detail)} />
      </div>
    </section>

    <Tabs label={$t('proxy.title')} bind:value={tab}
      items={[{ id: 'connect', label: $t('proxy.tab.connect') }, { id: 'share', label: $t('proxy.tab.share') }]} />

    {#if tab === 'connect'}
      {#if !vpn.running}
        <section class="card stack">
          <div class="card-head">
            <span class="ico"><Icon name="globe" size={16} /></span>
            <div class="grow"><h2>{$t('proxy.chooseExit')}</h2></div>
            {#if mode !== 'direct'}<Button size="sm" variant="ghost" icon="refresh" on:click={() => act('scan', () => loadExits(true))} disabled={!!busy}>{busy === 'scan' ? $t('proxy.searching') : $t('proxy.searchAgain')}</Button>{/if}
          </div>
          <Tabs variant="segment" label={$t('proxy.chooseExit')} bind:value={mode}
            items={[{ id: 'auto', label: $t('proxy.mode.auto') }, { id: 'manual', label: $t('proxy.mode.manual') }, { id: 'direct', label: $t('proxy.mode.direct') }]} />

          {#if mode === 'auto'}
            <div class="auto">
              <p class="small muted">{$t('proxy.autoBefore')}<b>{$t('proxy.checkSites')}</b>{$t('proxy.autoAfter')}</p>
              {#if best}
                <div class="exit pick-preview">
                  <span class="ex-ic"><Icon name="globe" size={16} /></span>
                  <span class="id mono">{whereExit(best)}</span>
                  <span class="chip ping {pingClass(best.latencyMs)}">{pingText(best.latencyMs)}</span>
                  <span class="chip">{best.pricePerMb ? price(best.pricePerMb, $lang) : $t('common.Free')}</span>
                  <span class="chip">{speed(best) ? mbit(best) : $t('proxy.speedUnknown')}</span>
                  <span class="chip">{sitesText(best)}</span>
                  <span class="grow"></span>
                  <span class="small faint">{$t('proxy.currentBest')}</span>
                </div>
              {:else}
                <p class="small faint row">{#if busy === 'scan'}<span class="spinner"></span> {$t('proxy.looking')}{:else}{$t('proxy.noExits')}{/if}</p>
              {/if}
            </div>
          {:else if mode === 'direct'}
            <div class="auto">
              <p class="small muted">{$t('proxy.directHint')}</p>
              <p class="small faint">{$t('proxy.externalExit')}</p>
              <Input bind:value={direct} aria-label={$t('proxy.directHint')} placeholder="echofog://join?… / echofog://relay/host:port/peer-id" on:keydown={(e) => e.key === 'Enter' && !blocked && connect()} />
              {#if isInvite && inviteErr}<p class="small error">{inviteErr}</p>
              {:else if isInvite && invite}<p class="small faint">{invite.private ? $t('access.private') : $t('access.publicNode')} · <span class="mono">{short(invite.peerId)}</span></p>
              {:else if direct.trim() && !directId && !isInvite}<p class="small error">{$t('proxy.directBad')}</p>{/if}
            </div>
          {:else}
            <div class="row small sortrow">
              <span class="faint">{$t('proxy.sort')}</span>
              {#each ['best', 'ping', 'price', 'speed'] as k}
                <button class="chip-btn" class:on={sortBy === k} aria-pressed={sortBy === k} on:click={() => (sortBy = k)}>{$t('proxy.sort.' + k)}</button>
              {/each}
            </div>
            <div class="exits" role="radiogroup" aria-label={$t('proxy.exits')}>
              {#each sorted as e (e.peerId)}
                <div class="exit" class:sel={selected === e.peerId} role="radio" aria-checked={selected === e.peerId} tabindex="0"
                  on:click={() => (selected = e.peerId)} on:keydown={(ev) => (ev.key === 'Enter' || ev.key === ' ') && (ev.preventDefault(), (selected = e.peerId))}>
                  <span class="radio"></span>
                  <span class="id mono">{short(e.peerId)}</span>
                  <span class="chip ping {pingClass(e.latencyMs)}" title={$t('proxy.pingTitle')}>{pingText(e.latencyMs)}</span>
                  <span class="chip" title={$t('proxy.priceTitle')}>{e.pricePerMb ? price(e.pricePerMb, $lang) : $t('common.Free')}</span>
                  <span class="chip" title={e.measuredKbps ? $t('proxy.measured') : $t('proxy.claimed')}>{speed(e) ? (e.measuredKbps ? '' : '~') + mbit(e) : $t('proxy.speedUnknown')}</span>
                  <span class="chip" title={$t('proxy.sitesTitle')}>{sitesText(e)}</span>
                  {#if e.paymentRequired}<span class="chip warn">{$t('proxy.needsPayment')}</span>{/if}
                  <span class="grow"></span>
                  <Button size="sm" variant="ghost" on:click={(ev) => { ev.stopPropagation(); probe(e.peerId); }} disabled={!!busy}>{busy === 'probe:' + e.peerId ? $t('proxy.testing') : $t('proxy.test')}</Button>
                </div>
              {:else}
                <p class="small faint row">{#if busy === 'scan'}<span class="spinner"></span> {$t('proxy.looking')}{:else}{$t('proxy.noExits')}{/if}</p>
              {/each}
            </div>
          {/if}

          {#if target && target.pricePerMb > 0}
            <div class="paid-ahead small">
              <b class="row"><Icon name="coin" size={14} /> {$t('proxy.paidAhead.title', { price: price(target.pricePerMb, $lang) })}</b>
              <TestnetNotice compact />
              <span>{$t('proxy.paidAhead.free', { kb: target.freeTestKb || 0 })} {#if mode === 'auto'}{$t('proxy.paidAhead.auto')}{/if}</span>
              <span>{$t('proxy.paidAhead.where')}
                <span class="tip" title={$t('proxy.pay.srcDepositTip')}><Icon name="info" size={13} /> {$t('proxy.pay.srcDeposit')}</span>
                <span class="tip" title={$t('proxy.pay.srcWalletTip')}><Icon name="info" size={13} /> {$t('proxy.pay.srcWallet')}</span>
              </span>
              <Checkbox checked={pay.autoPay} disabled={busy === 'autopay'} on:change={(e) => saveAutoPay(e.target.checked)} label={$t('proxy.paidAhead.autoPay', { cap: payForm.cap })} />
            </div>
          {/if}
        </section>
      {/if}

      {#if vpn.running && pay.active && Number(pay.pricePerGb) > 0}
        <section class="card stack">
          <div class="card-head">
            <span class="ico"><Icon name="coin" size={16} /></span>
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
          <TestnetNotice compact />
          {#if pay.needsPayment || pay.remainingGb <= 0}<p class="small notice warn">{$t('proxy.pay.needLead')}</p>{/if}
          <div class="row wrap paywith">
            <span class="small muted">{$t('proxy.pay.source')}</span>
            <span class="src" class:on={pay.paysByVault} title={$t('proxy.pay.srcDepositTip')}><Icon name="lock" size={13} /> {$t('proxy.pay.srcDeposit')} <Icon name="info" size={12} /></span>
            <span class="src" class:on={!pay.paysByVault} title={$t('proxy.pay.srcWalletTip')}><Icon name="wallet" size={13} /> {$t('proxy.pay.srcWallet')} <Icon name="info" size={12} /></span>
            <Button size="sm" variant="ghost" on:click={topUpDeposit}>{$t('proxy.pay.topUpDeposit')}</Button>
          </div>
          <details class="small muted diff">
            <summary>{$t('proxy.pay.diffTitle')}</summary>
            <p>{$t('proxy.pay.diffBody')}</p>
          </details>
          {#if pay.note}<p class="note">{pay.note}</p>{/if}
          {#if pay.lastError}<p class="small error">{pay.lastError}</p>{/if}
          <div class="row wrap paywith">
            <span class="small muted">{$t('proxy.pay.with')}</span>
            <Tabs variant="segment" label={$t('proxy.pay.with')} value={pay.payAsset} on:change={(e) => setAsset(e.detail)}
              items={[['auto', $t('proxy.pay.auto')], ['TON', 'TON'], ['USDT', 'USDT']].map(([k, label]) => ({ id: k, label, disabled: busy === 'asset' || (k === 'TON' && !pay.takesTon) }))} />
            {#if pay.chunkTon && pay.payAsset !== 'USDT' && pay.takesTon}<span class="small faint">{$t('proxy.pay.tonNow', { usd: payForm.chunk, ton: pay.chunkTon })}</span>{/if}
          </div>
          <div class="row wrap fields">
            <div class="grow"><Input label={$t('proxy.pay.chunk')} bind:value={payForm.chunk} on:input={() => (payTouched = true)} /></div>
            <div class="grow"><Input label={$t('proxy.pay.cap')} bind:value={payForm.cap} on:input={() => (payTouched = true)} /></div>
          </div>
          <Toggle label={$t('proxy.pay.autoPay')} checked={pay.autoPay} disabled={busy === 'autopay'} on:change={(e) => saveAutoPay(e.detail)} />
          <div class="row">
            <Button variant="primary" on:click={() => payNow(payForm.chunk)} disabled={!pay.canPay || busy === 'pay'} loading={busy === 'pay'}>
              {busy === 'pay' ? $t('proxy.pay.sending') : $t('proxy.pay.now', { amount: payForm.chunk })}
            </Button>
            {#if payTouched}<Button on:click={() => saveAutoPay(pay.autoPay)} disabled={!!busy}>{$t('proxy.pay.save')}</Button>{/if}
          </div>
        </section>
      {/if}

      <section class="card stack">
        <div class="card-head">
          <span class="ico"><Icon name="clock" size={16} /></span>
          <div class="grow">
            <h2>{$t('proxy.hist.title')}</h2>
            <p class="small muted">{$t('proxy.hist.subtitle')}</p>
          </div>
          {#if history.length}<Button size="sm" variant="ghost" on:click={clearHistory} disabled={!!busy}>{$t('proxy.hist.clear')}</Button>{/if}
        </div>
        {#if saved.length}
          <div class="hist">
            {#each shownExits as e (e.peerId)}
              <div class="hist-row" class:active={e.active} class:fav={e.favorite}>
                <div class="hist-main">
                  <span class="row">
                    <IconButton icon="star" size="sm" variant={e.favorite ? 'accent' : 'ghost'} pressed={e.favorite} label={e.favorite ? $t('proxy.hist.unstar') : $t('proxy.hist.star')} on:click={() => toggleStar(e)} disabled={!!busy} />
                    {#if renaming === e.peerId}
                      <!-- svelte-ignore a11y-autofocus -->
                      <input class="name-input" bind:value={nameDraft} maxlength="40" placeholder={$t('proxy.hist.namePlaceholder')} aria-label={$t('proxy.hist.rename')} autofocus
                        on:keydown={(ev) => { if (ev.key === 'Enter') saveName(e); if (ev.key === 'Escape') renaming = ''; }} on:blur={() => saveName(e)} />
                    {:else}
                      <b class:mono={!e.name && !contactName(e.peerId)} title={e.peerId}>{exitName(e)}</b>
                      <IconButton icon="edit" size="sm" label={$t('proxy.hist.rename')} on:click={() => startRename(e)} />
                    {/if}
                    {#if e.active}<span class="chip ok">{$t('proxy.hist.now')}</span>{/if}
                    {#if e.lastState && e.lastState !== 'ok'}<span class="chip warn">{$t('proxy.hist.state.' + e.lastState)}</span>{/if}
                  </span>
                  {#if e.countryCode}
                    <span class="small loc" title={e.addr}><Icon name="globe" size={12} /> {countryName(e.countryCode)}{e.city ? ', ' + e.city : ''}</span>
                  {/if}
                  <span class="small faint">
                    {e.lastUsed ? when(e.lastUsed, $lang) : $t('proxy.hist.never')}{e.addr ? ' · ' + e.addr : ''}
                    {#if e.sessions}· <button class="linkish" aria-expanded={openExit === e.peerId} on:click={() => (openExit = openExit === e.peerId ? '' : e.peerId)}>{$t('proxy.hist.sessions', { n: e.sessions })}</button>{/if}
                  </span>
                </div>
                <div class="hist-stats small">
                  <span title={$t('proxy.received')}><Icon name="download" size={12} /> {bytes(e.bytesRx, $lang)}</span>
                  <span title={$t('proxy.sent')}><Icon name="upload" size={12} /> {bytes(e.bytesTx, $lang)}</span>
                  {#if e.sessions}<span class="faint">{e.lastReadyMs ? $t('proxy.hist.ready', { s: (e.lastReadyMs / 1000).toFixed(1) }) : $t('proxy.hist.neverReady')}</span>{/if}
                  {#if e.refused && !refusals(e).known}<span class="faint" title={refusals(e).title}>{$t('proxy.hist.refused', { n: e.refused })}</span>{/if}
                  {#if refusals(e).exit}<span class="warn-text" title={refusals(e).title}>{$t('proxy.hist.exitFails', { n: refusals(e).exit })}</span>{/if}
                  {#if refusals(e).site}<span class="faint" title={refusals(e).title}>{$t('proxy.hist.siteFails', { n: refusals(e).site })}</span>{/if}
                  <span class="faint">{priceText(e)}</span>
                  {#if e.remainingAt && e.remaining >= 0}<span class="left" title={leftTitle(e)}>{leftText(e)}</span>{/if}
                </div>
                <div class="hist-actions">
                  <IconButton icon={copiedLink === e.peerId ? 'check' : 'copy'} size="sm" label={$t('proxy.hist.copyLink')} on:click={() => copyLink(e)} />
                  <IconButton icon="trash" size="sm" label={$t('proxy.hist.forget')} on:click={() => forget(e)} disabled={!!busy || e.active} />
                  {#if !(vpn.running && vpn.relayPeerId === e.peerId)}
                    <Button size="sm" on:click={() => connectTo(e)} disabled={!!busy}>{$t('proxy.hist.connect')}</Button>
                  {/if}
                </div>
                {#if openExit === e.peerId}
                  <div class="sessions small">
                    {#each sessionsOf(e.peerId) as h (h.id || h.peerId + h.startedAt)}
                      <div class="session">
                        <span>{when(h.startedAt, $lang)} · {duration(sessionSeconds(h))}{h.endReason ? ' · ' + $t('proxy.hist.end.' + h.endReason) : h.active ? ' · ' + $t('proxy.hist.now') : ''}</span>
                        <span class="faint"><Icon name="download" size={11} /> {bytes(h.bytesRx, $lang)} <Icon name="upload" size={11} /> {bytes(h.bytesTx, $lang)}{h.readyMs ? ' · ' + $t('proxy.hist.ready', { s: (h.readyMs / 1000).toFixed(1) }) : ''}{#if h.remainingAt && h.remaining >= 0}<span title={leftTitle(h)}> · {leftText(h)}</span>{/if}</span>
                      </div>
                    {/each}
                  </div>
                {/if}
              </div>
            {/each}
          </div>
          {#if saved.length > 6}
            <div><Button size="sm" variant="ghost" on:click={() => (showAllHistory = !showAllHistory)}>{showAllHistory ? $t('proxy.hist.showLess') : $t('proxy.hist.showAll', { n: saved.length })}</Button></div>
          {/if}
        {:else}
          <p class="small faint">{$t('proxy.hist.empty')}</p>
        {/if}
      </section>
    {:else}
      <section class="card stack" class:sharing={sharing.enabled}>
        <div class="card-head">
          <span class="ico" class:ok={sharing.enabled}><Icon name="upload" size={16} /></span>
          <div class="grow">
            <h2>{$t('proxy.share.title')} {#if sharing.enabled}<span class="chip ok live-chip"><span class="pulse"></span> {$t('proxy.share.live')}</span>{/if}</h2>
            <p class="small muted">{$t('proxy.share.hint')}</p>
          </div>
          <Button variant={sharing.enabled ? 'danger' : 'primary'} on:click={share} loading={busy === 'share'}>
            {busy === 'share' ? $t('common.working') : sharing.enabled ? $t('proxy.share.stop') : $t('proxy.share.start')}
          </Button>
        </div>
        {#if sharing.enabled}
          <div class="stats">
            <div class="stat"><span class="eyebrow">{$t('proxy.share.price')}</span><span class="num">{price(sharing.config.pricePerMb, $lang)}</span></div>
            <div class="stat"><span class="eyebrow">{$t('proxy.share.sessions')}</span><span class="num">{sharing.accepted} <span class="faint small">{$t('proxy.share.refused', { n: sharing.refused })}</span></span></div>
            <div class="stat"><span class="eyebrow">{$t('proxy.share.carried')}</span><span class="num">{bytes(sharing.bytes, $lang)}</span></div>
            {#if sharing.paymentsReady}<div class="stat"><span class="eyebrow">{$t('proxy.share.earned')}</span><span class="num ok">${sharing.earnedUsdt}</span></div>{/if}
          </div>
          {#if accessMode === 'public'}
            <p class="small faint">{$t('proxy.share.published', { when: sharing.lastPublish ? when(sharing.lastPublish, $lang) : $t('proxy.share.notYet') })}</p>
          {:else}
            <p class="small faint">{$t('access.sharedHidden')}</p>
          {/if}
          {#if sharing.lastError}<p class="small error">{sharing.lastError}</p>{/if}
        {/if}
        {#if (sharing.enabled && sharing.paymentsReady) || Number(vouchers.owedTon) > 0 || Number(vouchers.cashedTon) > 0}
          <div class="sub">
            <div class="eyebrow">{$t('proxy.share.deposits')}</div>
            <div class="stats">
              <div class="stat"><span class="eyebrow">{$t('proxy.share.owed')}</span><span class="num">{amount(vouchers.owedTon, $lang)} TON</span></div>
              <div class="stat"><span class="eyebrow">{$t('proxy.share.collected')}</span><span class="num ok">{amount(vouchers.cashedTon, $lang)} TON</span></div>
            </div>
            <TestnetNotice compact />
            {#if vouchers.claimProblem}
              <div class="small notice danger" role="alert">
                <Icon name="coin" size={14} />
                <span>{$t('proxy.share.noGas', { owed: amount(vouchers.owedTon, $lang) })}
                  {#if vouchers.walletAddr}<span class="block mono">{vouchers.walletAddr}
                    <IconButton icon="copy" size="sm" label={$t('common.copy')} on:click={() => copy(vouchers.walletAddr)} /></span>{/if}
                </span>
              </div>
            {/if}
            <p class="small muted">{$t('proxy.share.depositNote')}</p>
          </div>
        {/if}
        <ExitAccessCard access={sharing.access} enabled={sharing.enabled} on:change={refresh} />
        {#if !sharing.enabled && !sharing.access}
          <div class="row wrap fields">
            <div class="grow"><Input label={$t('proxy.share.priceLabel')} bind:value={form.price} /></div>
            <div class="grow"><Input type="number" label={$t('proxy.share.freeKb')} min="0" max="1024" bind:value={form.freeKb} /></div>
            <div class="grow"><Input type="number" label={$t('proxy.share.upload')} min="0" bind:value={form.mbps} /></div>
            <Button className="measure" on:click={measure} disabled={!!busy} loading={busy === 'measure'}>{busy === 'measure' ? $t('proxy.share.measuring') : $t('proxy.share.measure')}</Button>
          </div>
          <p class="note small">
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
      </section>

      <AccessCard {contacts} />

      <RelayCard {contacts} />
    {/if}
  </div>
</div>

{#if checkingSites}
  <SiteCheck exit={checkExit} exitLabel={checkLabel} on:close={() => { checkingSites = false; loadExits(false).catch(() => {}); }} />
{/if}

<style>
  .px { height: 100%; overflow-y: auto; padding: var(--sp-6) var(--sp-6) var(--sp-12); display: block; }
  .pxwrap { max-width: 880px; margin: 0 auto; display: flex; flex-direction: column; gap: var(--sp-3); }
  .head { display: flex; align-items: flex-end; gap: var(--sp-3); flex-wrap: wrap; }
  .banner { display: flex; align-items: center; gap: var(--sp-3); padding: var(--sp-3) var(--sp-4); border-radius: var(--r-lg); }
  .banner.warn { background: var(--warn-soft); color: var(--text); border: 1px solid var(--warn); }
  .banner.warn :global(svg) { color: var(--warn-fg); }
  .notice { display: flex; gap: 6px; align-items: flex-start; border-radius: var(--r-md); padding: 8px 10px; line-height: 1.4; }
  .notice :global(svg) { flex: none; margin-top: 2px; }
  .notice.warn { color: var(--warn-fg); background: var(--warn-soft); }
  .notice.danger { color: var(--danger-fg); background: var(--danger-soft); }
  .block { display: block; margin-top: 2px; }
  .sub { display: flex; flex-direction: column; gap: var(--sp-2); }
  .fields { align-items: flex-end; }
  .fields .grow { min-width: 150px; }
  :global(.measure) { align-self: flex-end; }
  .paywith { align-items: center; gap: var(--sp-3); }

  /* the state card: a square mark that changes colour, the title, one main button */
  .conn-top { display: flex; align-items: center; gap: var(--sp-4); flex-wrap: wrap; }
  .state-ic {
    width: 44px; height: 44px; border-radius: var(--r-lg); flex: none; display: grid; place-items: center;
    background: var(--surface-2); color: var(--muted); border: 1px solid var(--border);
    transition: background var(--dur) var(--ease), color var(--dur) var(--ease);
  }
  .conn.running .state-ic { background: var(--warn-soft); color: var(--warn-fg); border-color: transparent; }
  .conn.ready .state-ic { background: var(--ok-soft); color: var(--ok-fg); }
  .conn.running.problem .state-ic { background: var(--danger-soft); color: var(--danger-fg); }
  .conn .grow h2 { font-size: var(--fs-lg); line-height: var(--lh-lg); }
  .opts { display: flex; flex-direction: column; gap: var(--sp-3); padding-top: var(--sp-3); border-top: 1px solid var(--border); }
  .last-exit { gap: var(--sp-3); flex-wrap: wrap; }

  .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: var(--sp-2); }
  .stat { display: flex; flex-direction: column; gap: 2px; padding: var(--sp-2) var(--sp-3); border-radius: var(--r-md); background: var(--surface-2); min-width: 0; }
  .num { display: inline-flex; align-items: center; gap: 6px; font-weight: var(--fw-semibold); font-size: var(--fs-md); font-variant-numeric: tabular-nums; }
  .num.ok { color: var(--ok-fg); }

  .auto { display: flex; flex-direction: column; gap: var(--sp-3); }
  .exits { display: flex; flex-direction: column; gap: var(--sp-1); }
  .exit {
    display: flex; align-items: center; gap: var(--sp-2); padding: 8px var(--sp-3); border-radius: var(--r-md); flex-wrap: wrap;
    background: var(--surface-2); border: 1px solid transparent; cursor: pointer;
    transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease);
  }
  .exit:hover { border-color: var(--border-strong); }
  .exit:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: 1px; }
  .exit.sel { border-color: var(--accent); background: var(--accent-soft); }
  .pick-preview { cursor: default; }
  .pick-preview:hover { border-color: transparent; }
  .ex-ic { width: 28px; height: 28px; border-radius: var(--r-md); display: grid; place-items: center; background: var(--accent-soft); color: var(--accent-fg); }
  .radio { width: 16px; height: 16px; border-radius: 50%; border: 2px solid var(--border-strong); flex: none; box-sizing: border-box; }
  .exit.sel .radio { border: 5px solid var(--accent); }
  .id { font-size: var(--fs-sm); min-width: 90px; word-break: normal; }
  .chip.ping.good { color: var(--ok-fg); background: var(--ok-soft); }
  .chip.ping.mid { color: var(--warn-fg); background: var(--warn-soft); }
  .chip.ping.bad { color: var(--danger-fg); background: var(--danger-soft); }
  .paid-ahead { display: flex; flex-direction: column; gap: 6px; padding: var(--sp-3); border-radius: var(--r-md); background: var(--warn-soft); line-height: 1.45; }
  .paid-ahead b { gap: 6px; color: var(--warn-fg); }
  .tip { display: inline-flex; align-items: center; gap: 3px; margin-left: 6px; text-decoration: underline dotted; cursor: help; }
  .src { display: inline-flex; align-items: center; gap: 4px; padding: 2px 10px; border-radius: var(--r-pill); border: 1px solid var(--border); color: var(--muted); font-size: var(--fs-xs); cursor: help; }
  .src.on { color: var(--accent-fg); background: var(--accent-soft); border-color: transparent; }
  .diff summary { cursor: pointer; }
  .diff p { margin: 6px 0 0; line-height: 1.45; }
  .sortrow { gap: var(--sp-1); }
  .chip-btn { height: 24px; padding: 0 10px; font-size: var(--fs-xs); border-radius: var(--r-pill); background: transparent; border-color: var(--border); color: var(--muted); }
  .chip-btn.on, .chip-btn.on:hover:not(:disabled) { background: var(--accent-soft); color: var(--accent-fg); border-color: transparent; }

  .hist { display: flex; flex-direction: column; gap: var(--sp-1); }
  .hist-row { display: grid; grid-template-columns: minmax(0, 1.3fr) minmax(0, 2fr) auto; gap: var(--sp-3); align-items: center; padding: var(--sp-2) var(--sp-3); border: 1px solid var(--border); border-radius: var(--r-md); }
  .hist-row.active { border-color: var(--ok); background: var(--ok-soft); }
  .hist-row.fav:not(.active) { border-color: var(--accent-soft); }
  .hist-main { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
  .hist-main .faint { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .hist-main .row { flex-wrap: wrap; gap: 4px 6px; }
  .hist-main b { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 100%; font-weight: var(--fw-semibold); }
  .hist-stats { display: flex; flex-wrap: wrap; gap: 4px 12px; align-items: center; }
  .hist-stats span { display: inline-flex; align-items: center; gap: 4px; }
  .hist-stats .left { color: var(--text); font-weight: var(--fw-medium); }
  .hist-stats .warn-text { color: var(--warn-fg); }
  .hist-actions { display: flex; gap: var(--sp-1); align-items: center; }
  .loc { display: inline-flex; align-items: center; gap: 4px; color: var(--muted); }
  .name-input { padding: 3px 8px; font-size: var(--fs-sm); min-width: 0; width: 100%; max-width: 260px; height: var(--ctl-sm); }
  .linkish { padding: 0; height: auto; border: none; background: none; color: var(--accent-fg); font: inherit; cursor: pointer; }
  .linkish:hover:not(:disabled) { background: none; border: none; text-decoration: underline; }
  .sessions { grid-column: 1 / -1; display: flex; flex-direction: column; gap: 4px; padding-top: 6px; border-top: 1px dashed var(--border); }
  .session { display: flex; justify-content: space-between; gap: var(--sp-3); flex-wrap: wrap; }
  .session span { display: inline-flex; align-items: center; gap: 4px; }
  @media (max-width: 720px) { .hist-row { grid-template-columns: 1fr; } }

  .live-chip { margin-left: 6px; vertical-align: middle; font-size: var(--fs-2xs); }
  .pulse { width: 6px; height: 6px; border-radius: 50%; background: currentColor; animation: pulse 1.4s ease-in-out infinite; }
  @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }
</style>
