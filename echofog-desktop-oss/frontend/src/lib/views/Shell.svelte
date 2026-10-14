<script>
  import { onMount, onDestroy } from 'svelte';
  import { App } from '../api.js';
  import { profile } from '../stores/session.js';
  import { chats } from '../stores/chats.js';
  import { isCallActive, initCallEvents } from '../stores/call.store.js';
  import { loadPeerDirectory } from '../stores/peers.store.js';
  import { view } from '../stores/nav.js';
  import { bindSounds } from '../soundsBind.js';
  import CallOverlay from '../components/ui/CallOverlay.svelte';
  import Chats from './Chats.svelte';
  import Proxy from './Proxy.svelte';
  import Wallet from './Wallet.svelte';
  import Settings from './Settings.svelte';
  import Icon from '../components/ui/Icon.svelte';

  const views = [
    { id: 'chats', label: 'Chats', icon: 'message' },
    { id: 'proxy', label: 'Proxy', icon: 'shield' },
    { id: 'wallet', label: 'Wallet', icon: 'wallet' },
    { id: 'settings', label: 'Settings', icon: 'settings' },
  ];
  let net = { peerCount: 0, status: 'OFFLINE' };
  let poll;

  $: unread = $chats.reduce((n, c) => n + (c.unread || 0), 0);

  async function refresh() {
    try { net = await App.GetNetworkStatus(); } catch (_) { /* keep last */ }
  }

  let offSounds = () => {};

  onMount(() => {
    offSounds = bindSounds();
    initCallEvents();
    loadPeerDirectory();
    refresh();
    poll = setInterval(refresh, 5000);
  });
  onDestroy(() => { clearInterval(poll); offSounds(); });
</script>

<div class="shell">
  <nav>
    <div class="brand">
      <span class="mark" aria-hidden="true"></span>
      <span>EchoFog</span>
    </div>
    {#each views as v}
      <button class="ghost item" class:active={$view === v.id} on:click={() => view.set(v.id)}>
        <Icon name={v.icon} size={18} />
        <span class="grow">{v.label}</span>
        {#if v.id === 'chats' && $isCallActive}<span class="live" title="In a call"></span>{/if}
        {#if v.id === 'chats' && unread}<span class="badge">{unread}</span>{/if}
      </button>
    {/each}
    <div class="grow" />
    <div class="status small">
      <div class="row"><span class="dot" class:on={net.status === 'ONLINE' && net.peerCount > 0} />
        <span>{net.status === 'ONLINE' ? `${net.peerCount} peer${net.peerCount === 1 ? '' : 's'}` : 'Offline'}</span></div>
      <div class="faint profile">{$profile}</div>
    </div>
  </nav>

  <main>
    <div class="view">
      <!-- Chats stays mounted so a running call keeps its video elements -->
      <div class="fill" hidden={$view !== 'chats'}><Chats /></div>
      {#if $view === 'proxy'}<Proxy />{/if}
      {#if $view === 'wallet'}<Wallet />{/if}
      {#if $view === 'settings'}<Settings />{/if}
    </div>
  </main>
</div>

<CallOverlay />

<style>
  .shell { display: flex; height: 100%; }
  nav {
    width: 200px; flex: none; display: flex; flex-direction: column; gap: 3px;
    padding: 16px 12px; background: var(--panel); border-right: 1px solid var(--line-soft);
  }
  .brand { display: flex; align-items: center; gap: 10px; font-weight: 700; font-size: 15px; padding: 2px 8px 18px; letter-spacing: -0.01em; }
  .mark {
    width: 26px; height: 26px; border-radius: 8px; flex: none;
    background: radial-gradient(circle at 30% 30%, rgba(255, 255, 255, 0.55), transparent 45%), linear-gradient(135deg, var(--accent), var(--accent-2));
    box-shadow: 0 4px 14px rgba(123, 140, 255, 0.35);
  }
  .item {
    position: relative; display: flex; gap: 11px; align-items: center; text-align: left;
    padding: 9px 11px; color: var(--muted); font-weight: 500;
  }
  .item:hover:not(:disabled) { color: var(--text); }
  .item.active, .item.active:hover:not(:disabled) { background: var(--accent-soft); color: var(--text); }
  .item.active :global(svg) { color: var(--accent); }
  .item.active::before {
    content: ''; position: absolute; left: -12px; top: 9px; bottom: 9px; width: 3px; border-radius: 0 3px 3px 0;
    background: linear-gradient(var(--accent), var(--accent-2));
  }
  .live { width: 8px; height: 8px; border-radius: 50%; background: var(--ok); box-shadow: 0 0 0 0 rgba(62, 207, 142, 0.6); animation: live 1.8s infinite; }
  @keyframes live { 0% { box-shadow: 0 0 0 0 rgba(62, 207, 142, 0.55); } 70% { box-shadow: 0 0 0 7px rgba(62, 207, 142, 0); } 100% { box-shadow: 0 0 0 0 rgba(62, 207, 142, 0); } }
  .status {
    padding: 10px 12px; display: flex; flex-direction: column; gap: 4px; color: var(--muted);
    background: var(--raised); border-radius: var(--radius); border: 1px solid var(--line-soft);
  }
  .profile { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  main { flex: 1; min-width: 0; display: flex; flex-direction: column; }
  .view { flex: 1; min-height: 0; position: relative; }
  .fill { height: 100%; }
  .fill[hidden] { display: none; }
</style>
