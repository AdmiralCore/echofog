<script>
  import { onMount, onDestroy } from 'svelte';
  import { App, openURL, on } from '../api.js';
  import { openLinkDialog, looksLikeLink } from '../links.js';
  import OpenLink from '../components/OpenLink.svelte';
  import CrashDialog from '../components/CrashDialog.svelte';
  import { profile } from '../stores/session.js';
  import { chats } from '../stores/chats.js';
  import { isCallActive, initCallEvents } from '../stores/call.store.js';
  import { loadPeerDirectory } from '../stores/peers.store.js';
  import { view, focus } from '../stores/nav.js';
  import { anon, refreshAnon } from '../stores/anon.js';
  import { fly } from 'svelte/transition';
  import { netStatus } from '../stores/net.js';
  import { resolvedTheme, toggleTheme } from '../stores/theme.js';
  import { displayName } from '../contact.js';
  import { bindSounds } from '../soundsBind.js';
  import CallOverlay from '../components/ui/CallOverlay.svelte';
  import Chats from './Chats.svelte';
  import Proxy from './Proxy.svelte';
  import Wallet from './Wallet.svelte';
  import Settings from './Settings.svelte';
  import Logs from './Logs.svelte';
  import Icon from '../components/ui/Icon.svelte';
  import Avatar from '../components/ui/Avatar.svelte';
  import SpacesRail from '../features/chat/SpacesRail.svelte';
  import { t } from '../i18n/index.js';

  const views = [
    { id: 'chats', label: 'shell.chats', icon: 'message' },
    { id: 'proxy', label: 'shell.proxy', icon: 'shield' },
    { id: 'wallet', label: 'shell.wallet', icon: 'wallet' },
    { id: 'logs', label: 'shell.logs', icon: 'terminal' },
    { id: 'settings', label: 'shell.settings', icon: 'settings' },
  ];
  const ITEM = 38; // item height + gap, for the sliding highlight

  // a newer release on GitHub: one quiet banner, until the user closes it for that version
  const UPDATE_SEEN = 'echofog.updateDismissed';
  const TELEGRAM = 'https://t.me/EchoFogDev';
  let update = null;
  async function checkUpdate() {
    try {
      const u = await App.CheckForUpdate();
      let seen = '';
      try { seen = localStorage.getItem(UPDATE_SEEN) || ''; } catch (_) { /* private mode */ }
      if (u?.newer && u.latest !== seen) update = u;
    } catch (_) { /* offline or GitHub unreachable: nothing to say */ }
  }
  function dismissUpdate() {
    try { localStorage.setItem(UPDATE_SEEN, update.latest); } catch (_) { /* private mode */ }
    update = null;
  }

  // the last run crashed: a banner until the user opens the report or closes it
  let crash = null;
  let crashOpen = false;
  async function checkCrash() {
    try { crash = await App.LastCrash(); } catch (_) { /* an older core: nothing to say */ }
  }
  function forgetCrash() {
    crash = null;
    crashOpen = false;
    App.ForgetLastCrash().catch(() => {});
  }

  let net = { peerCount: 0, status: 'OFFLINE' };
  let poll;
  let width = 1200;

  // the sections sit on a narrow icon rail (the space goes to the channels, the call and the chat);
  // a very narrow window moves it to a bottom tab bar
  $: bottom = width < 640;
  // the rail is narrow (icons) unless the user widened it (names beside the icons); a window under 980 px
  // keeps it narrow whatever was chosen. The choice is remembered under the old key: '0' = wide
  let wideNav = false;
  try { wideNav = localStorage.getItem('echofog.navCollapsed') === '0'; } catch (_) {}
  function toggleWide() {
    wideNav = !wideNav;
    try { localStorage.setItem('echofog.navCollapsed', wideNav ? '0' : '1'); } catch (_) {}
  }
  $: rail = !bottom && !(wideNav && width >= 980);
  $: showSpaces = !bottom;
  $: activeIndex = Math.max(0, views.findIndex((v) => v.id === $view));
  $: myName = ($displayName || '').trim() || $profile || $t('shell.me');
  $: unread = $chats.reduce((n, c) => n + (c.unread || 0), 0);
  $: online = net.status === 'ONLINE' && net.peerCount > 0;

  // offline for a while after start (the built-in nodes may be blocked): offer to add a node by hand
  let offlineSince = Date.now();
  let now = Date.now();
  let bannerHidden = false;
  $: if (online) offlineSince = now;
  $: showOffline = !online && !bannerHidden && $view !== 'settings' && now - offlineSince > 20000;
  function addNode() {
    focus.set('bootstrap');
    view.set('settings');
  }
  $: statusText = net.status === 'ONLINE' ? $t('shell.peers', { n: net.peerCount }) : $t('shell.offline');

  async function refresh() {
    try { net = await App.GetNetworkStatus(); netStatus.set(net); } catch (_) { /* keep last */ }
  }

  let offSounds = () => {};
  let offLink = () => {};

  // EchoFog links: Ctrl+L opens the dialog; a link pasted outside a text field, a link clicked in a
  // browser while the app runs, and the link that started the app all open it with the link in it
  const typing = (el) => el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName));
  function onKey(e) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'l') {
      e.preventDefault();
      openLinkDialog('');
    }
  }
  function onPaste(e) {
    if (typing(document.activeElement)) return;
    const text = e.clipboardData?.getData('text') || '';
    if (!looksLikeLink(text)) return;
    e.preventDefault();
    openLinkDialog(text.trim());
  }

  onMount(() => {
    offLink = on('link:open', (link) => openLinkDialog(link || ''));
    App.TakeStartLink().then((link) => { if (link) openLinkDialog(link); }).catch(() => {});
    checkUpdate();
    checkCrash();
    refreshAnon();
    offSounds = bindSounds();
    initCallEvents();
    loadPeerDirectory();
    refresh();
    poll = setInterval(() => { now = Date.now(); refresh(); }, 5000);
  });
  onDestroy(() => { clearInterval(poll); offSounds(); offLink(); });
</script>

<svelte:window bind:innerWidth={width} on:keydown={onKey} on:paste={onPaste} />
<OpenLink />
{#if crash && crashOpen}<CrashDialog {crash} on:close={forgetCrash} />{/if}

<div class="shell" class:bottom>
  <nav class:rail class:bottom aria-label={$t('shell.sections')}>
    {#if !bottom}
      <div class="brand">
        <span class="mark" aria-hidden="true"><span></span></span>
        <span class="name">EchoFog</span>
      </div>
    {/if}

    <div class="items">
      {#if !bottom}<span class="indicator" style="transform: translateY({activeIndex * ITEM}px)" aria-hidden="true"></span>{/if}
      {#each views as v}
        <button class="item" class:active={$view === v.id} title={rail || bottom ? $t(v.label) : ''} aria-current={$view === v.id ? 'page' : undefined} on:click={() => view.set(v.id)}>
          <span class="ic">
            <Icon name={v.icon} size={18} />
            {#if v.id === 'chats' && unread && (rail || bottom)}<span class="pip">{unread > 99 ? '99+' : unread}</span>{/if}
            {#if v.id === 'chats' && $isCallActive}<span class="live" title={$t('shell.inCall')}></span>{/if}
          </span>
          <span class="label">{$t(v.label)}</span>
          {#if v.id === 'chats' && unread && !rail && !bottom}<span class="badge">{unread}</span>{/if}
        </button>
      {/each}
    </div>

    {#if showSpaces}
      <div class="spaces-slot"><SpacesRail wide={!rail} /></div>
    {/if}

    {#if !bottom}
      <div class="grow" />
      <div class="foot">
        {#if width >= 980}
          <button class="theme widen" title={rail ? $t('shell.expandSidebar') : $t('shell.collapseSidebar')} aria-label={rail ? $t('shell.expandSidebar') : $t('shell.collapseSidebar')} aria-pressed={!rail} on:click={toggleWide}>
            <Icon name={rail ? 'chevronRight' : 'chevronLeft'} size={17} />
            <span class="label">{$t('shell.collapse')}</span>
          </button>
        {/if}
        <button class="theme" title={$resolvedTheme === 'dark' ? $t('shell.lightTheme') : $t('shell.darkTheme')} aria-label={$t('shell.toggleTheme')} on:click={toggleTheme}>
          <span class="swap" class:dark={$resolvedTheme === 'dark'}>
            <span class="i sun"><Icon name="sun" size={17} /></span>
            <span class="i moon"><Icon name="moon" size={17} /></span>
          </span>
          <span class="label">{$resolvedTheme === 'dark' ? $t('shell.darkTheme') : $t('shell.lightTheme')}</span>
        </button>
        <button class="me" title={rail ? `${myName} · ${statusText}${$anon && $anon.running && !$anon.broken ? ' · ' + $t('anon.badge.' + $anon.seenBy) : ''}` : $t('shell.settings')} on:click={() => view.set('settings')}>
          <span class="av">
            <Avatar id={$profile} name={myName} size={32} />
            <span class="presence" class:on={online}></span>
          </span>
          <span class="who">
            <span class="nm">{myName}</span>
            <span class="st" class:on={online}>{statusText}</span>
            {#if $anon && $anon.running && !$anon.broken}<span class="anon" title={$t('anon.now.' + $anon.seenBy)}><Icon name="eyeOff" size={12} /> {$t('anon.badge.' + $anon.seenBy)}</span>{/if}
          </span>
        </button>
      </div>
    {/if}
  </nav>

  <main>
    <div class="banners">
    {#if crash && !crashOpen}
      <div class="banner crash" role="alert" transition:fly={{ y: -16, duration: 300 }}>
        <span class="off-ic crash-ic"><Icon name="info" size={17} /></span>
        <div class="grow off-text">
          <b>{$t('crash.banner')}</b>
          <span class="mono">{crash.reason}</span>
        </div>
        <button class="primary small" on:click={() => (crashOpen = true)}>{$t('crash.details')}</button>
        <button class="icon off-x" title={$t('boot.dismiss')} aria-label={$t('boot.dismiss')} on:click={forgetCrash}><Icon name="close" size={15} /></button>
      </div>
    {/if}
    {#if update}
      <div class="banner update" role="status" transition:fly={{ y: -16, duration: 300 }}>
        <span class="off-ic up-ic"><Icon name="download" size={17} /></span>
        <div class="grow off-text">
          <b>{$t('update.title', { v: update.latest })}</b>
          <span>{$t('update.text', { current: update.current })}</span>
        </div>
        <button class="primary small" on:click={() => openURL(update.url)}>{$t('update.download')}</button>
        <button class="small tg" on:click={() => openURL(TELEGRAM)}>{$t('update.telegram')}</button>
        <button class="icon off-x" title={$t('boot.dismiss')} aria-label={$t('boot.dismiss')} on:click={dismissUpdate}><Icon name="close" size={15} /></button>
      </div>
    {/if}
    {#if showOffline}
      <div class="banner offline" role="status" transition:fly={{ y: -16, duration: 300 }}>
        <span class="off-ic"><Icon name="globe" size={17} /></span>
        <div class="grow off-text">
          <b>{$t('boot.offlineTitle')}</b>
          <span>{$t('boot.offlineText')}</span>
        </div>
        <button class="primary small" on:click={addNode}>{$t('boot.offlineAction')}</button>
        <button class="icon off-x" title={$t('boot.dismiss')} aria-label={$t('boot.dismiss')} on:click={() => (bannerHidden = true)}><Icon name="close" size={15} /></button>
      </div>
    {/if}
    </div>
    <!-- Chats stays mounted so a running call keeps its video elements -->
    <div class="fill" hidden={$view !== 'chats'}><Chats /></div>
    {#key $view}
      {#if $view !== 'chats'}
        <div class="fill enter">
          {#if $view === 'proxy'}<Proxy />{/if}
          {#if $view === 'wallet'}<Wallet />{/if}
          {#if $view === 'logs'}<Logs />{/if}
          {#if $view === 'settings'}<Settings />{/if}
        </div>
      {/if}
    {/key}
  </main>
</div>

<CallOverlay />

<style>
  .shell { display: flex; height: 100%; background: var(--bg); }
  .shell.bottom { flex-direction: column-reverse; }

  nav {
    width: 56px; transition: width var(--dur-slow) var(--ease); flex: none; display: flex; flex-direction: column; gap: 2px;
    padding: 12px 8px 8px; background: var(--surface-1); border-right: 1px solid var(--border);
    overflow: hidden; position: relative; z-index: 2;
  }

  .brand { display: flex; align-items: center; gap: 10px; height: 36px; padding: 0 2px 0 6px; margin-bottom: 10px; white-space: nowrap; }
  .name { flex: 1; font-weight: var(--fw-bold); font-size: var(--fs-md); letter-spacing: -0.01em; transition: opacity var(--dur); }
  nav.rail .name { display: none; }
  .mark {
    width: 24px; height: 24px; border-radius: var(--r-md); flex: none; position: relative; overflow: hidden; background: var(--accent);
  }
  .mark span {
    position: absolute; inset: 5px; border-radius: 50%;
    border: 2px solid var(--accent-ink); border-right-color: transparent; transform: rotate(-30deg);
  }
  .mark::after { content: ''; position: absolute; width: 4px; height: 4px; border-radius: 50%; background: var(--accent-ink); left: 10px; top: 10px; }
  nav.rail .brand { margin-bottom: 18px; padding-left: 7px; }

  .items { position: relative; display: flex; flex-direction: column; gap: 2px; }
  .indicator {
    position: absolute; left: 0; right: 0; top: 0; height: 36px; border-radius: var(--r-md);
    background: var(--surface-3); transition: transform var(--dur-slow) var(--ease);
  }
  .indicator::before {
    content: ''; position: absolute; left: -8px; top: 8px; bottom: 8px; width: 3px; border-radius: 0 3px 3px 0; background: var(--accent);
  }
  .item {
    position: relative; display: flex; gap: 12px; align-items: center; height: 36px; padding: 0 10px;
    background: transparent; border: none; color: var(--muted); font-weight: var(--fw-medium); font-size: var(--fs-md); text-align: left; border-radius: var(--r-md);
  }
  .item:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .item.active, .item.active:hover:not(:disabled) { background: transparent; color: var(--text); }
  .item.active .ic { color: var(--accent-fg); }
  .ic { position: relative; display: grid; place-items: center; flex: none; }
  .label { flex: 1; min-width: 0; white-space: nowrap; overflow: hidden; transition: opacity var(--dur); }
  nav.rail .label { display: none; }
  nav.rail .badge { display: none; }
  .badge { min-width: 18px; height: 18px; padding: 0 5px; border-radius: var(--r-pill); background: var(--accent); color: var(--accent-ink); font-size: var(--fs-2xs); font-weight: var(--fw-bold); display: inline-grid; place-items: center; }
  .pip {
    position: absolute; top: -6px; right: -9px; min-width: 16px; height: 16px; padding: 0 4px; border-radius: var(--r-pill);
    background: var(--accent); color: var(--accent-ink); font-size: 10px; font-weight: var(--fw-bold); display: grid; place-items: center;
    box-shadow: 0 0 0 2px var(--surface-1);
  }
  .live {
    position: absolute; bottom: -2px; right: -3px; width: 8px; height: 8px; border-radius: 50%;
    background: var(--ok); box-shadow: 0 0 0 2px var(--surface-1); animation: live 1.8s infinite;
  }
  @keyframes live { 50% { opacity: 0.4; } }

  nav:not(.rail):not(.bottom) { width: 208px; }
  .spaces-slot { margin-top: 10px; padding-top: 10px; border-top: 1px solid var(--border); min-height: 0; flex: 0 1 auto; overflow-y: auto; overflow-x: hidden; scrollbar-width: none; }
  .spaces-slot::-webkit-scrollbar { display: none; }
  .foot { display: flex; flex-direction: column; gap: 2px; padding-top: 8px; border-top: 1px solid var(--border); }
  .theme, .me {
    display: flex; align-items: center; gap: 12px; padding: 0 10px; height: 36px; border: none; background: transparent;
    color: var(--muted); font-weight: var(--fw-medium); font-size: var(--fs-md); text-align: left; border-radius: var(--r-md); white-space: nowrap; overflow: hidden;
  }
  .theme:hover:not(:disabled), .me:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .swap { position: relative; width: 17px; height: 17px; flex: none; }
  .swap .i { position: absolute; inset: 0; display: grid; place-items: center; transition: opacity var(--dur); }
  .swap .moon { opacity: 0; }
  .swap.dark .sun { opacity: 0; }
  .swap.dark .moon { opacity: 1; }
  .me { height: 48px; padding: 0 4px; gap: 10px; }
  nav.rail .me { padding: 0 3px; }
  .av { position: relative; flex: none; }
  .presence { position: absolute; right: -1px; bottom: -1px; width: 10px; height: 10px; border-radius: 50%; background: var(--faint); border: 2px solid var(--surface-1); }
  .presence.on { background: var(--ok); }
  .who { display: flex; flex-direction: column; min-width: 0; line-height: 1.25; transition: opacity var(--dur); }
  nav.rail .who, nav.rail .theme .label { display: none; }
  .nm { color: var(--text); font-weight: var(--fw-semibold); font-size: var(--fs-sm); overflow: hidden; text-overflow: ellipsis; }
  .st { font-size: var(--fs-xs); color: var(--faint); font-weight: var(--fw-regular); }
  .st.on { color: var(--ok-fg); }

  /* bottom tab bar */
  nav.bottom {
    width: 100%; height: 56px; flex-direction: row; padding: 4px 8px; border-right: none; border-top: 1px solid var(--border);
    background: var(--surface-1);
  }
  nav.bottom .items { flex-direction: row; flex: 1; justify-content: space-around; gap: 2px; }
  nav.bottom .item { flex: 1; flex-direction: column; justify-content: center; gap: 2px; height: 48px; padding: 0; font-size: var(--fs-2xs); }
  nav.bottom .label { flex: none; opacity: 1; }
  nav.bottom .item.active { color: var(--accent-fg); }

  .banners {
    position: absolute; z-index: 20; top: 12px; left: 50%; transform: translateX(-50%);
    width: min(680px, calc(100% - 24px)); display: flex; flex-direction: column; gap: 8px; pointer-events: none;
  }
  .banner {
    pointer-events: auto; display: flex; align-items: center; gap: 12px; padding: 8px 8px 8px 12px;
    border-radius: var(--r-xl); background: var(--surface-2);
    border: 1px solid color-mix(in srgb, var(--warn) 45%, transparent); box-shadow: var(--shadow-md);
  }
  .banner.crash { border-color: color-mix(in srgb, var(--danger) 50%, transparent); }
  .crash-ic { background: var(--danger-soft) !important; color: var(--danger-fg) !important; }
  .banner.crash .mono { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .banner.update { border-color: color-mix(in srgb, var(--accent) 45%, transparent); }
  .up-ic { background: var(--accent-soft) !important; color: var(--accent-fg) !important; }
  .tg { background: var(--surface-3); color: var(--text); border: 1px solid var(--border); }
  .off-ic { width: 28px; height: 28px; border-radius: var(--r-md); flex: none; display: grid; place-items: center; background: var(--warn-soft); color: var(--warn-fg); }
  .off-text { display: flex; flex-direction: column; font-size: var(--fs-xs); color: var(--muted); line-height: var(--lh-xs); }
  .off-text b { color: var(--text); font-size: var(--fs-sm); font-weight: var(--fw-semibold); }
  .off-x { width: var(--ctl-sm); height: var(--ctl-sm); }
  main { flex: 1; min-width: 0; min-height: 0; position: relative; display: flex; flex-direction: column; }
  .fill { position: absolute; inset: 0; }
  .fill[hidden] { display: none; }
  .enter { animation: view-in var(--dur-slow) var(--ease) both; }
  @keyframes view-in { from { opacity: 0; } to { opacity: 1; } }
  .anon { display: inline-flex; align-items: center; gap: 4px; font-size: var(--fs-2xs); color: var(--ok-fg); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
</style>
