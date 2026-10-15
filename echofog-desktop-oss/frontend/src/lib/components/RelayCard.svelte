<script>
  // The Relay card: whether this computer relays for others (public, private for a few people, or
  // only helping peers connect directly), the relay link to give out, and the relays this app uses.
  import { onMount, onDestroy } from 'svelte';
  import QRCode from 'qrcode';
  import { App, errText, copy } from '../api.js';
  import { bytes, short } from '../format.js';
  import { parseRelayLink } from '../contact.js';
  import { me } from '../stores/session.js';
  import Icon from './ui/Icon.svelte';
  import PeerList from './PeerList.svelte';
  import QRScan from '../views/QRScan.svelte';

  export let contacts = [];

  const MODES = [
    { id: 'off', label: 'Off', text: 'This computer relays nothing for others. Your own calls and messages still work.' },
    { id: 'public', label: 'Public', text: 'Anyone may relay calls and messages through this computer when they cannot reach each other directly, and offline messages wait here. This helps the network.' },
    { id: 'private', label: 'Private', text: 'Only the people you list may use this relay, and it is not advertised. Give them your relay link.' },
    { id: 'signaling', label: 'Signaling only', text: 'Helps peers behind NATs find a direct path to each other, but carries no calls, files or messages.' },
  ];

  let st = null;
  let error = '';
  let busy = '';
  let copied = '';
  let addInput = '';
  let addError = '';
  let port = '';
  let qr = '';
  let showQR = false;
  let scanning = false;
  let timer;

  const modeOf = (s) => (s.signalingOnly ? 'signaling' : !s.server ? 'off' : s.private ? 'private' : 'public');
  $: mode = st ? modeOf(st.settings) : 'public';
  $: modeInfo = MODES.find((m) => m.id === mode);
  $: relays = mode === 'public' || mode === 'private';

  async function load() {
    try {
      st = await App.GetRelayStatus();
      if (port === '') port = String(st.settings.listenPort || 0);
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }

  async function save(patch, what = 'save') {
    busy = what;
    error = '';
    try {
      await App.SetRelaySettings({ ...st.settings, ...patch });
      await load();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }

  function setMode(id) {
    if (id === mode) return;
    save({ server: id !== 'off', private: id === 'private', signalingOnly: id === 'signaling' }, 'mode');
  }

  async function addRelay(text = addInput) {
    addError = '';
    if (!parseRelayLink(text)) {
      addError = 'Paste a relay link (echofog://relay/…) or host:port/<peer id>.';
      return;
    }
    busy = 'add';
    try {
      await App.AddRelay(text.trim());
      addInput = '';
      await load();
    } catch (err) {
      addError = errText(err);
    } finally {
      busy = '';
    }
  }

  function scanned(e) {
    scanning = false;
    addRelay(e.detail);
  }

  async function copyText(text, which) {
    copied = (await copy(text)) ? which : '';
    setTimeout(() => (copied = ''), 1500);
  }

  async function toggleQR() {
    showQR = !showQR;
    if (showQR && st?.link) qr = await QRCode.toDataURL(st.link, { margin: 1, width: 180, color: { dark: '#000000', light: '#ffffff' } });
  }

  const savePort = () => save({ listenPort: Math.round(Number(port) || 0) }, 'port');

  onMount(() => {
    load();
    timer = setInterval(load, 10000);
  });
  onDestroy(() => clearInterval(timer));
</script>

<div class="card stack">
  <div class="card-head">
    <span class="ico" class:ok={st && mode !== 'off'}><Icon name="relay" size={18} /></span>
    <div class="grow">
      <h2>Relay
        {#if st && mode !== 'off'}<span class="chip ok live-chip">{modeInfo.label.toLowerCase()}</span>{/if}
        {#if st && !st.chosen}<span class="chip warn live-chip" title="You have not changed this yet">default</span>{/if}
      </h2>
      <p class="small muted">Carry calls and messages for people who cannot reach each other directly, and choose the relays you use. This is free: nobody pays for relaying.</p>
    </div>
  </div>
  {#if error}<p class="small error">{error}</p>{/if}

  {#if st}
    <div class="seg" role="radiogroup" aria-label="Relay mode">
      {#each MODES as m}
        <button role="radio" aria-checked={mode === m.id} class:active={mode === m.id} disabled={!!busy} on:click={() => setMode(m.id)}>{m.label}</button>
      {/each}
    </div>
    <p class="small muted">{modeInfo.text}</p>
    {#if !st.chosen && mode === 'public'}
      <p class="note">By default every EchoFog desktop is a public relay, so that people behind strict routers can still call each other. It uses some of your bandwidth while others need it. Pick <b>Off</b> or <b>Private</b> if you would rather not.</p>
    {/if}

    {#if mode === 'private'}
      <div class="sub">
        <div class="eyebrow">Allowed people</div>
        <PeerList ids={st.settings.allowedPeers} {contacts} disabled={!!busy} label="Allowed people"
          empty="Nobody yet: the relay serves no one until you add people."
          on:change={(e) => save({ allowedPeers: e.detail }, 'peers')} />
      </div>
    {/if}

    {#if relays}
      <div class="sub">
        <div class="eyebrow">Your relay link</div>
        {#if st.link}
          <div class="copybox">
            <span class="mono grow">{st.link}</span>
            <button class="icon" title="Copy link" on:click={() => copyText(st.link, 'link')}><Icon name={copied === 'link' ? 'check' : 'copy'} size={16} /></button>
            <button class="icon" class:on={showQR} title="Show QR code" on:click={toggleQR}><Icon name="qr" size={16} /></button>
          </div>
          {#if showQR && qr}<img class="qr" src={qr} alt="Relay link QR code" width="180" height="180" />{/if}
          <p class="small faint">
            {st.mapped ? 'Reachable through the port your router forwards.' : 'This computer has a public address.'}
            {mode === 'private' ? 'Only the people above can use it.' : 'Anyone with the link can use it.'}
          </p>
        {:else}
          <p class="note">{st.reason || 'Others cannot reach this computer yet.'}</p>
        {/if}
        <div class="row wrap portrow">
          <label class="port">Listen port (0 = a new one at every start)
            <input type="number" min="0" max="65535" bind:value={port} />
          </label>
          <button on:click={savePort} disabled={!!busy || String(st.settings.listenPort) === String(port)}>{busy === 'port' ? 'Saving…' : 'Save port'}</button>
        </div>
        {#if st.restartNeeded}<p class="small warn">The new port is used after you restart the app.</p>{:else if !st.settings.listenPort}<p class="small faint">With a random port the link changes every time the app starts. Fix a port to keep it.</p>{/if}
        <p class="small faint">Your peer ID, for someone who runs a private relay: <span class="mono">{$me.id || '…'}</span>
          {#if $me.id}<button class="icon tiny" title="Copy peer ID" on:click={() => copyText($me.id, 'id')}><Icon name={copied === 'id' ? 'check' : 'copy'} size={13} /></button>{/if}</p>
      </div>

      <div class="stats">
        <div class="stat"><span class="eyebrow">Relaying now</span><span class="num">{st.activeCircuits}</span></div>
        <div class="stat"><span class="eyebrow">Carried</span><span class="num">{bytes(st.bytesRouted)}</span></div>
        <div class="stat"><span class="eyebrow">Messages passed on</span><span class="num">{st.forwarded}</span></div>
        {#if mode === 'private'}<div class="stat"><span class="eyebrow">Turned away</span><span class="num">{st.accessRefused}</span></div>{/if}
      </div>
    {/if}

    <div class="sub">
      <div class="eyebrow">Relays you use</div>
      <p class="small muted">The app finds relays by itself. Add a relay someone gave you (for example their private one) and it is used first.</p>
      {#each st.using as r (r.peerId)}
        <div class="relay-row">
          <span class="dot" class:on={r.connected} title={r.connected ? 'Connected' : 'Not connected'}></span>
          <span class="mono grow">{r.addr} <span class="faint">· {short(r.peerId)}</span></span>
          <span class="small {r.connected ? 'ok' : 'faint'}">{r.connected ? 'connected' : 'connecting…'}</span>
          <button class="icon tiny" title="Remove" disabled={!!busy} on:click={() => save({ relays: st.settings.relays.filter((x) => !x.endsWith('/' + r.peerId)) }, 'relays')}><Icon name="trash" size={14} /></button>
        </div>
      {/each}
      {#if scanning}
        <QRScan on:scan={scanned} on:close={() => (scanning = false)} />
      {/if}
      <div class="row wrap">
        <input class="grow link-in" bind:value={addInput} placeholder="echofog://relay/host:port/peer-id" on:keydown={(e) => e.key === 'Enter' && addRelay()} />
        <button class="primary" on:click={() => addRelay()} disabled={!!busy || !addInput.trim()}>{busy === 'add' ? 'Adding…' : 'Add relay'}</button>
        <button class="with-icon" on:click={() => (scanning = true)}><Icon name="qr" size={15} /> Scan</button>
      </div>
      {#if addError}<p class="small error">{addError}</p>{/if}
    </div>
  {:else if !error}
    <p class="small faint row"><span class="spinner"></span> Loading…</p>
  {/if}
</div>

<style>
  .live-chip { margin-left: 6px; vertical-align: middle; font-size: 11px; }
  .sub { display: flex; flex-direction: column; gap: 8px; padding-top: 4px; }
  .copybox { display: flex; align-items: center; gap: 6px; padding: 8px 6px 8px 12px; border-radius: 12px; background: var(--raised); }
  .qr { border-radius: 12px; align-self: flex-start; background: #fff; padding: 6px; }
  .portrow { align-items: flex-end; }
  .port { max-width: 280px; flex: 1; }
  .icon.tiny { width: 24px; height: 24px; border-radius: 7px; vertical-align: middle; }
  .link-in { min-width: 220px; }
  .relay-row { display: flex; align-items: center; gap: 10px; padding: 8px 10px; border-radius: 12px; background: var(--raised); }
  .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 10px; }
  .stat { display: flex; flex-direction: column; gap: 4px; padding: 12px 14px; border-radius: 14px; background: var(--raised); min-width: 0; }
  .num { font-weight: 650; font-size: 15px; font-variant-numeric: tabular-nums; }
  .seg button:disabled { opacity: 0.6; }
</style>
