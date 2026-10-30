<script>
  import { onMount, onDestroy } from 'svelte';
  import { App, openURL, on } from '../api.js';
  import { openLinkDialog, looksLikeLink } from '../links.js';
  import OpenLink from '../components/OpenLink.svelte';
  import { profile } from '../stores/session.js';
  import { chats } from '../stores/chats.js';
  import { isCallActive, initCallEvents } from '../stores/call.store.js';
  import { loadPeerDirectory } from '../stores/peers.store.js';
  import { view, focus } from '../stores/nav.js';
  import { anon, refreshAnon } from '../stores/anon.js';
  import { fly } from 'svelte/transition';
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
  import { t } from '../i18n/index.js';

  const views = [
    { id: 'chats', label: 'shell.chats', icon: 'message' },
    { id: 'proxy', label: 'shell.proxy', icon: 'shield' },
    { id: 'wallet', label: 'shell.wallet', icon: 'wallet' },
    { id: 'logs', label: 'shell.logs', icon: 'terminal' },
    { id: 'settings', label: 'shell.settings', icon: 'settings' },
  ];
  const ITEM = 44; // item height + gap, for the sliding highlight

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

  let net = { peerCount: 0, status: 'OFFLINE' };
  let poll;
  let width = 1200;

  // the apps panel can be collapsed to a narrow icon rail; the choice is remembered
  let navCollapsed = false;
  try { navCollapsed = localStorage.getItem('echofog.navCollapsed') === '1'; } catch (_) {}
  function toggleNav() {
    navCollapsed = !navCollapsed;
    try { localStorage.setItem('echofog.navCollapsed', navCollapsed ? '1' : '0'); } catch (_) {}
  }

  // narrow windows fold the panel to a rail, very narrow ones move it to a bottom tab bar
  $: bottom = width < 640;
  $: rail = !bottom && (navCollapsed || width < 980);
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
    try { net = await App.GetNetworkStatus(); } catch (_) { /* keep last */ }
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

<div class="shell" class:bottom>
  <nav class:rail class:bottom aria-label={$t('shell.sections')}>
    {#if !bottom}
      <div class="brand">
        <span class="mark" aria-hidden="true"><span></span></span>
        <span class="name">EchoFog</span>
        {#if width >= 980}
          <button class="collapse" title={navCollapsed ? $t('shell.expand') : $t('shell.collapse')} aria-label={navCollapsed ? $t('shell.expandSidebar') : $t('shell.collapseSidebar')} on:click={toggleNav}>
            <Icon name={navCollapsed ? 'chevronRight' : 'chevronLeft'} size={15} stroke={2.2} />
          </button>
        {/if}
      </div>
    {/if}

    <div class="items">
      {#if !bottom}<span class="indicator" style="transform: translateY({activeIndex * ITEM}px)" aria-hidden="true"></span>{/if}
      {#each views as v}
        <button class="item" class:active={$view === v.id} title={rail || bottom ? $t(v.label) : ''} aria-current={$view === v.id ? 'page' : undefined} on:click={() => view.set(v.id)}>
          <span class="ic">
            <Icon name={v.icon} size={19} />
            {#if v.id === 'chats' && unread && (rail || bottom)}<span class="pip">{unread > 99 ? '99+' : unread}</span>{/if}
            {#if v.id === 'chats' && $isCallActive}<span class="live" title={$t('shell.inCall')}></span>{/if}
          </span>
          <span class="label">{$t(v.label)}</span>
          {#if v.id === 'chats' && unread && !rail && !bottom}<span class="badge">{unread}</span>{/if}
        </button>
      {/each}
    </div>

    {#if !bottom}
      <div class="grow" />
      <div class="foot">
        <button class="theme" title={$resolvedTheme === 'dark' ? $t('shell.lightTheme') : $t('shell.darkTheme')} aria-label={$t('shell.toggleTheme')} on:click={toggleTheme}>
          <span class="swap" class:dark={$resolvedTheme === 'dark'}>
            <span class="i sun"><Icon name="sun" size={17} /></span>
            <span class="i moon"><Icon name="moon" size={17} /></span>
          </span>
          <span class="label">{$resolvedTheme === 'dark' ? $t('shell.darkTheme') : $t('shell.lightTheme')}</span>
        </button>
        <button class="me" title={rail ? `${myName} · ${statusText}` : $t('shell.settings')} on:click={() => view.set('settings')}>
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
    width: 236px; flex: none; display: flex; flex-direction: column; gap: 4px;
    padding: 14px 12px 12px; background: var(--panel); border-right: 1px solid var(--line-soft);
    overflow: hidden; position: relative; z-index: 2;
  }
  nav:not(.bottom) { transition: width 0.32s var(--ease), padding 0.32s var(--ease); }
  nav.rail { width: 72px; padding: 14px 12px 12px; }

  .brand { display: flex; align-items: center; gap: 11px; height: 40px; padding: 0 4px 0 6px; margin-bottom: 12px; white-space: nowrap; }
  .name { flex: 1; font-weight: 750; font-size: 16px; letter-spacing: -0.02em; transition: opacity 0.2s; }
  nav.rail .name { opacity: 0; pointer-events: none; }
  .mark {
    width: 30px; height: 30px; border-radius: 10px; flex: none; position: relative; overflow: hidden;
    background: linear-gradient(135deg, var(--accent), var(--accent-2)); box-shadow: var(--glow);
  }
  .mark span {
    position: absolute; inset: 7px; border-radius: 50%;
    border: 2.5px solid rgba(255, 255, 255, 0.95); border-right-color: transparent; transform: rotate(-30deg);
  }
  .mark::after { content: ''; position: absolute; width: 6px; height: 6px; border-radius: 50%; background: #fff; left: 12px; top: 12px; }
  .collapse {
    width: 26px; height: 26px; padding: 0; border-radius: 8px; flex: none; color: var(--faint);
    display: grid; place-items: center; background: transparent; border: none;
  }
  .collapse:hover:not(:disabled) { color: var(--text); background: var(--hover); border: none; }
  nav.rail .collapse { position: absolute; left: 50%; top: 56px; transform: translateX(-50%); opacity: 0; transition: opacity 0.2s; }
  nav.rail:hover .collapse { opacity: 1; }
  nav.rail .brand { margin-bottom: 30px; }

  .items { position: relative; display: flex; flex-direction: column; gap: 4px; }
  .indicator {
    position: absolute; left: 0; right: 0; top: 0; height: 40px; border-radius: 12px;
    background: var(--accent-soft); transition: transform 0.45s var(--spring);
  }
  .indicator::before {
    content: ''; position: absolute; left: -12px; top: 10px; bottom: 10px; width: 3px; border-radius: 0 3px 3px 0;
    background: linear-gradient(var(--accent), var(--accent-2));
  }
  .item {
    position: relative; display: flex; gap: 12px; align-items: center; height: 40px; padding: 0 12px;
    background: transparent; border: none; color: var(--muted); font-weight: 520; text-align: left; border-radius: 12px;
  }
  .item:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .item.active, .item.active:hover:not(:disabled) { background: transparent; color: var(--text); }
  .item.active .ic { color: var(--accent); }
  .ic { position: relative; display: grid; place-items: center; flex: none; transition: transform 0.35s var(--spring), color 0.2s; }
  .item:hover .ic { transform: scale(1.08); }
  .label { flex: 1; min-width: 0; white-space: nowrap; overflow: hidden; transition: opacity 0.2s; }
  nav.rail .label { opacity: 0; }
  nav.rail .badge { display: none; }
  .pip {
    position: absolute; top: -7px; right: -10px; min-width: 17px; height: 17px; padding: 0 4px; border-radius: 999px;
    background: var(--accent); color: #fff; font-size: 10px; font-weight: 750; display: grid; place-items: center;
    box-shadow: 0 0 0 2px var(--panel); animation: pop 0.35s var(--spring);
  }
  .live {
    position: absolute; bottom: -2px; right: -3px; width: 8px; height: 8px; border-radius: 50%;
    background: var(--ok); box-shadow: 0 0 0 2px var(--panel); animation: live 1.8s infinite;
  }
  @keyframes live { 0% { box-shadow: 0 0 0 2px var(--panel), 0 0 0 2px var(--ok-soft); } 70% { box-shadow: 0 0 0 2px var(--panel), 0 0 0 8px transparent; } 100% { box-shadow: 0 0 0 2px var(--panel), 0 0 0 2px transparent; } }

  .foot { display: flex; flex-direction: column; gap: 4px; padding-top: 10px; border-top: 1px solid var(--line-soft); }
  .theme, .me {
    display: flex; align-items: center; gap: 12px; padding: 0 12px; height: 40px; border: none; background: transparent;
    color: var(--muted); font-weight: 520; text-align: left; border-radius: 12px; white-space: nowrap; overflow: hidden;
  }
  .theme:hover:not(:disabled), .me:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .swap { position: relative; width: 19px; height: 19px; flex: none; }
  .swap .i { position: absolute; inset: 0; display: grid; place-items: center; transition: transform 0.5s var(--spring), opacity 0.3s; }
  .swap .moon { transform: rotate(-90deg) scale(0.4); opacity: 0; }
  .swap.dark .sun { transform: rotate(90deg) scale(0.4); opacity: 0; }
  .swap.dark .moon { transform: none; opacity: 1; }
  .me { height: 52px; padding: 0 6px; gap: 10px; }
  .av { position: relative; flex: none; }
  .presence { position: absolute; right: -1px; bottom: -1px; width: 11px; height: 11px; border-radius: 50%; background: var(--faint); border: 2.5px solid var(--panel); transition: background 0.3s; }
  .presence.on { background: var(--ok); }
  .who { display: flex; flex-direction: column; min-width: 0; line-height: 1.25; transition: opacity 0.2s; }
  nav.rail .who, nav.rail .theme .label { opacity: 0; }
  .nm { color: var(--text); font-weight: 600; font-size: 13px; overflow: hidden; text-overflow: ellipsis; }
  .st { font-size: 11.5px; color: var(--faint); font-weight: 450; }
  .st.on { color: var(--ok); }

  /* bottom tab bar */
  nav.bottom {
    width: 100%; height: 62px; flex-direction: row; padding: 6px 8px; border-right: none; border-top: 1px solid var(--line-soft);
    background: var(--glass); backdrop-filter: blur(18px) saturate(1.4);
  }
  nav.bottom .items { flex-direction: row; flex: 1; justify-content: space-around; gap: 2px; }
  nav.bottom .item { flex: 1; flex-direction: column; justify-content: center; gap: 2px; height: 50px; padding: 0; font-size: 11px; }
  nav.bottom .label { flex: none; opacity: 1; }
  nav.bottom .item.active .ic { transform: translateY(-1px) scale(1.1); }
  nav.bottom .item.active { color: var(--accent); }

  .banners {
    position: absolute; z-index: 20; top: 12px; left: 50%; transform: translateX(-50%);
    width: min(680px, calc(100% - 24px)); display: flex; flex-direction: column; gap: 8px; pointer-events: none;
  }
  .banner {
    pointer-events: auto; display: flex; align-items: center; gap: 12px; padding: 10px 10px 10px 14px;
    border-radius: 16px; background: var(--glass); backdrop-filter: blur(18px) saturate(1.4);
    border: 1px solid color-mix(in srgb, var(--warn) 35%, transparent); box-shadow: var(--shadow);
  }
  .banner.update { border-color: color-mix(in srgb, var(--accent) 40%, transparent); }
  .up-ic { background: var(--accent-soft) !important; color: var(--accent) !important; }
  .tg { background: var(--raised); color: var(--text); border: 1px solid var(--line-soft); }
  .off-ic { width: 32px; height: 32px; border-radius: 10px; flex: none; display: grid; place-items: center; background: var(--warn-soft); color: var(--warn); }
  .off-text { display: flex; flex-direction: column; font-size: 12.5px; color: var(--muted); line-height: 1.35; }
  .off-text b { color: var(--text); font-size: 13.5px; }
  .off-x { width: 30px; height: 30px; }
  main { flex: 1; min-width: 0; min-height: 0; position: relative; display: flex; flex-direction: column; }
  .fill { position: absolute; inset: 0; }
  .fill[hidden] { display: none; }
  .enter { animation: view-in 0.38s var(--ease) both; }
  @keyframes view-in { from { opacity: 0; transform: translateY(8px) scale(0.995); } to { opacity: 1; transform: none; } }
  .anon { display: inline-flex; align-items: center; gap: 4px; font-size: 11px; color: #38c172; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
</style>
