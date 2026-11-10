<script>
  import { onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import QRCode from 'qrcode';
  import { me, profile } from '../stores/session.js';
  import { openLinkDialog } from '../links.js';
  import { chats, activeId, activeChannel, messages, activeGroupCalls, loadChats, openChat, addContact, createGroup, joinGroup } from '../stores/chats.js';
  import { isCallActive, peerInfo, stageExpanded } from '../stores/call.store.js';
  import CallStage from '../components/call/CallStage.svelte';
  import { errText, copy } from '../api.js';
  import { when } from '../format.js';
  import { t, lang } from '../i18n/index.js';
  import { makeContactLink, parseContactInput, displayName, currentDisplayName } from '../contact.js';
  import Conversation from './Conversation.svelte';
  import Saved from './Saved.svelte';
  import { saved, loadSaved, SAVED_ID } from '../stores/saved.js';
  import { loadTransfers } from '../stores/files.js';
  import QRScan from './QRScan.svelte';
  import Avatar from '../components/ui/Avatar.svelte';
  import Icon from '../components/ui/Icon.svelte';
  import Popover from '../components/ui/Popover.svelte';
  import Skeleton from '../components/ui/Skeleton.svelte';
  import SelfCard from '../features/chat/SelfCard.svelte';
  import { chatsAction, view } from '../stores/nav.js';
  import { panelHidden, togglePanel, setSelfCard, personalCardOnScreen } from '../stores/panels.js';
  import PanelHost from '../features/chat/PanelHost.svelte';
  import IconButton from '../components/ui/IconButton.svelte';

  let panel = ''; // '' | 'contact' | 'group' | 'join' | 'me'
  let contactInput = '';
  let name = '';
  let groupKind = 'group';
  let invite = '';
  let busy = false;
  let error = '';
  let copied = '';
  let scanning = false;
  let qr = '';

  // the chat list can be hidden (a strip by the rail, PanelHost); the choice is remembered apart for the
  // personal list and for spaces
  const listHidden = panelHidden.personal;
  // Ctrl+B hides or shows the column of what is open: the channels of a space, or the personal list
  function onKey(e) {
    if (!(e.ctrlKey || e.metaKey) || e.altKey || e.shiftKey || e.key.toLowerCase() !== 'b') return;
    if ($view !== 'chats' || narrow) return;
    e.preventDefault();
    togglePanel(isSpace ? 'space' : 'personal');
  }

  // the list can be resized by dragging its edge; the width is remembered
  let listWidth = 320;
  try { listWidth = Number(localStorage.getItem('echofog.chatsWidth')) || 320; } catch (_) {}
  let resizing = null;
  function startResize(e) {
    resizing = { x: e.clientX, w: listWidth };
    document.body.style.cursor = 'col-resize';
  }
  function onResize(e) {
    if (!resizing) return;
    listWidth = Math.round(Math.max(250, Math.min(460, resizing.w + e.clientX - resizing.x)));
  }
  function endResize() {
    if (!resizing) return;
    resizing = null;
    document.body.style.cursor = '';
    try { localStorage.setItem('echofog.chatsWidth', String(listWidth)); } catch (_) {}
  }

  // a narrow window shows either the list or the open chat, like a phone
  let width = 1000;
  $: narrow = width < 700;
  // under 640 px the window has no rail of spaces (a bottom bar instead), so the list brings them back
  let winW = 1200;
  $: noRail = winW < 640;

  let query = '';
  $: q = query.trim().toLowerCase();
  $: sorted = [...$chats]
    .filter((c) => !q || (c.name || '').toLowerCase().includes(q) || (c.last || '').toLowerCase().includes(q))
    .sort((a, b) => (b.ts || 0) - (a.ts || 0));
  // the rail of spaces shows the spaces, so the list holds the people only; a search looks through
  // everything, and a window with no rail gets the spaces back
  $: spaces = sorted.filter((c) => c.kind === 'group');
  $: people = sorted.filter((c) => c.kind !== 'group');
  $: withSpaces = noRail || !!q;
  $: sections = [
    ...(withSpaces ? [{ key: 'spaces', title: $t('chats.sectionSpaces'), list: spaces }] : []),
    { key: 'people', title: $t('chats.sectionPeople'), list: people },
  ].filter((s) => s.list.length);

  // the one "new" button: a menu of what can be started
  let newBtn;
  let newMenu = false;
  function startNew(p) {
    newMenu = false;
    toggle(p);
  }
  $: active = $chats.find((c) => c.id === $activeId);
  $: showSaved = $activeId === SAVED_ID;
  $: opened = !!active || showSaved;
  $: savedRow = !q || $t('saved.title').toLowerCase().includes(q);
  $: showCall = $isCallActive && !!active && $peerInfo.chatId === $activeId && $stageExpanded;
  let maximized = false; // "theatre": the stage takes the whole window beside the rail
  $: if (!showCall) maximized = false;
  // a space shows its own column of channels instead of this list (its header switches chats)
  $: isSpace = !!active && active.kind === 'group';
  $: showAside = narrow ? !opened : !isSpace;
  // this column carries the "you" card at its foot, so the rail leaves its own card away while it is
  // on screen (folded to a strip, it is not; a narrow window forces it in place)
  $: setSelfCard('personal', personalCardOnScreen($view, showAside, $listHidden, narrow));

  // the chat beside a call opens and closes; the choice is remembered, and without one the chat is open
  // only when the window is wide enough to leave the picture its width
  let chatPref = null;
  try { const v = localStorage.getItem('echofog.call.chat'); chatPref = v === '1' ? true : v === '0' ? false : null; } catch (_) {}
  $: chatOpen = chatPref ?? width >= 1800;
  function toggleCallChat() {
    chatPref = !chatOpen;
    try { localStorage.setItem('echofog.call.chat', chatPref ? '1' : '0'); } catch (_) {}
  }
  // messages that came to the room chat while it was closed: a count on the chat button of the stage
  let seen = new Set();
  let baseKey = '';
  let baseAt = 0;
  let missed = 0;
  function trackMissed(list, open, stage, key) {
    const closed = stage && !open;
    if (!closed || key !== baseKey) {
      // a baseline: what is there now is not missed
      baseKey = closed ? key : '';
      baseAt = Date.now();
      seen = new Set(list.map((m) => m.id));
      missed = 0;
      return;
    }
    for (const m of list) {
      if (seen.has(m.id)) continue;
      seen.add(m.id);
      // a list that is still loading after a switch is not news
      if (!m.mine && !m.system && Date.now() - baseAt > 1500) missed++;
    }
  }
  $: trackMissed($messages, chatOpen, showCall, $activeId + '/' + $activeChannel);
  // a voice room's call is named after the room, a chat's after the person or the group
  const stageTitle = (c) => ($peerInfo.room ? String($peerInfo.alias || '').split(' \u00b7 ').pop() : c.name);
  $: parsed = parseContactInput(contactInput);
  $: myLink = $me.pubKey ? makeContactLink(currentDisplayName($profile), $me.pubKey) : '';

  // the list is a skeleton until the first load answers
  let listLoaded = false;
  onMount(() => {
    Promise.resolve(loadChats()).finally(() => (listLoaded = true));
    loadSaved();
    loadTransfers('');
  });

  async function toggle(p) {
    panel = panel === p ? '' : p;
    contactInput = name = invite = error = '';
    scanning = false;
    if (panel === 'me' && myLink) {
      qr = await QRCode.toDataURL(myLink, { margin: 1, width: 200, color: { dark: '#000000', light: '#ffffff' } });
    }
  }

  // "+" on the spaces rail: the list shows with the form of what was asked for
  $: if ($chatsAction) {
    const what = $chatsAction;
    chatsAction.set('');
    activeId.set('');
    listHidden.set(false);
    panel = '';
    toggle(what);
  }

  async function run(fn) {
    busy = true;
    error = '';
    try {
      await fn();
      panel = '';
      contactInput = name = invite = '';
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  function submitContact() {
    if (!parsed) { error = $t('chats.err.notContact'); return; }
    const finalName = name.trim() || parsed.name;
    if (!finalName) { error = $t('chats.err.noName'); return; }
    run(() => addContact(parsed.key, finalName));
  }

  function scanned(e) {
    scanning = false;
    contactInput = e.detail;
    if (!parseContactInput(contactInput)) error = $t('chats.err.notEchoFog');
  }

  async function copyText(what, text) {
    if (await copy(text)) {
      copied = what;
      setTimeout(() => (copied = ''), 1500);
    }
  }
</script>

<svelte:window on:mousemove={onResize} on:mouseup={endResize} on:keydown={onKey} bind:innerWidth={winW} />

<div class="chats" class:narrow bind:clientWidth={width} class:resizing={!!resizing}>
  {#if showAside}
  <PanelHost kind="personal" off={narrow} label={$t('panel.pin')}>
  <aside style={narrow ? '' : `width:${listWidth}px`}>
    <div class="head">
      <h2 class="grow">{$t('chats.title')}</h2>
      <button class="icon new" bind:this={newBtn} class:on={newMenu || ['contact', 'group', 'join'].includes(panel)} title={$t('chats.new')} aria-label={$t('chats.new')} on:click={() => (newMenu = !newMenu)}><Icon name="edit" size={17} /></button>
      <button class="icon" class:on={panel === 'me'} title={$t('chats.myContact')} aria-label={$t('chats.myContact')} on:click={() => toggle('me')}><Icon name="qr" size={17} /></button>
      {#if !narrow}<IconButton icon="panel" label={$t($listHidden ? 'panel.pin' : 'panel.hide') + ' · Ctrl+B'} pressed={$listHidden} size="sm" on:click={() => togglePanel('personal')} />{/if}
    </div>

    {#if newMenu}
      <Popover anchor={newBtn} width={250} label={$t('chats.new')} on:close={() => (newMenu = false)}>
        <div class="newmenu">
          <button class="nm-item" on:click={() => startNew('contact')}><Icon name="userPlus" size={17} /><span>{$t('chats.newContact')}</span></button>
          <button class="nm-item" on:click={() => startNew('group')}><Icon name="users" size={17} /><span>{$t('chats.newSpace')}</span></button>
          <button class="nm-item" on:click={() => startNew('join')}><Icon name="link" size={17} /><span>{$t('chats.joinInvite')}</span></button>
          <button class="nm-item" on:click={() => { newMenu = false; openLinkDialog(''); }}><Icon name="link" size={17} /><span>{$t('links.title')}</span><kbd class="small faint">Ctrl+L</kbd></button>
        </div>
      </Popover>
    {/if}

    {#if panel}
      <div class="sheet" transition:slide={{ duration: 260 }}>
        <div class="sheet-in">
          <div class="row sheet-head">
            <span class="eyebrow grow">{$t('chats.panel.' + panel)}</span>
            <button class="icon tiny" aria-label={$t('common.close')} on:click={() => toggle(panel)}><Icon name="close" size={14} stroke={2.2} /></button>
          </div>

          {#if panel === 'contact'}
            {#if scanning}
              <QRScan on:scan={scanned} on:close={() => (scanning = false)} />
            {:else}
              <form class="stack tight" on:submit|preventDefault={submitContact}>
                <textarea rows="3" placeholder={$t('chats.pasteContact')} bind:value={contactInput} spellcheck="false" />
                <input placeholder={parsed?.name ? $t('chats.nameWithHint', { name: parsed.name }) : $t('chats.name')} bind:value={name} />
                <div class="row">
                  <button class="primary grow" type="submit" disabled={busy || !contactInput.trim()}>{$t('chats.addContact')}</button>
                  <button type="button" class="with-icon" on:click={() => (scanning = true)}><Icon name="qr" size={15} /> {$t('chats.scan')}</button>
                </div>
              </form>
            {/if}
          {:else if panel === 'group'}
            <form class="stack tight" on:submit|preventDefault={() => run(() => createGroup(name, groupKind))}>
              <input placeholder={$t('chats.groupName')} bind:value={name} />
              <div class="seg kind">
                <button type="button" class:active={groupKind === 'group'} on:click={() => (groupKind = 'group')}>{$t('chats.group')}</button>
                <button type="button" class:active={groupKind === 'channel'} on:click={() => (groupKind = 'channel')}>{$t('chats.channel')}</button>
              </div>
              <p class="small faint">{groupKind === 'group' ? $t('chats.groupHint') : $t('chats.channelHint')}</p>
              <button class="primary" type="submit" disabled={busy || !name.trim()}>{$t('common.create')}</button>
            </form>
          {:else if panel === 'join'}
            <form class="stack tight" on:submit|preventDefault={() => run(() => joinGroup(invite))}>
              <textarea rows="3" placeholder={$t('chats.pasteInvite')} bind:value={invite} spellcheck="false" />
              <button class="primary" type="submit" disabled={busy || !invite.trim()}>{$t('common.join')}</button>
            </form>
          {:else if panel === 'me'}
            <div class="stack tight">
              <p class="small muted">{$t('chats.meHint', { name: currentDisplayName($profile) })}</p>
              {#if qr}<img class="qr" src={qr} alt={$t('chats.qrAlt')} />{/if}
              <div class="row">
                <button class="grow with-icon center" on:click={() => copyText('link', myLink)}><Icon name={copied === 'link' ? 'check' : 'link'} size={15} />{copied === 'link' ? $t('common.copied') : $t('chats.copyLink')}</button>
                <button class="grow with-icon center" on:click={() => copyText('key', $me.pubKey)}><Icon name={copied === 'key' ? 'check' : 'key'} size={15} />{copied === 'key' ? $t('common.copied') : $t('chats.copyKey')}</button>
              </div>
            </div>
          {/if}
          {#if error}<p class="error small">{error}</p>{/if}
        </div>
      </div>
    {/if}

    <div class="search">
      <Icon name="search" size={15} />
      <input placeholder={$t('common.search')} bind:value={query} spellcheck="false" />
      {#if query}<button class="icon tiny" aria-label={$t('chats.clearSearch')} on:click={() => (query = '')}><Icon name="close" size={13} stroke={2.2} /></button>{/if}
    </div>

    <div class="list">
      {#if savedRow}
        <button class="conv" class:sel={showSaved} on:click={() => activeId.set(SAVED_ID)}>
          <div class="avatar"><span class="saved-av"><Icon name="bookmark" size={18} /></span></div>
          <div class="body">
            <div class="row"><span class="grow name">{$t('saved.title')}</span></div>
            <div class="row"><span class="grow last">{$saved.length ? $t('saved.count', { n: $saved.length }) : $t('saved.hint')}</span></div>
          </div>
        </button>
      {/if}
      {#each sections as sec (sec.key)}
      {#if sections.length > 1}<div class="section">{sec.title}<span class="cnt">{sec.list.length}</span></div>{/if}
      {#each sec.list as c (c.id)}
        <button class="conv" class:sel={c.id === $activeId} class:unread={c.unread} on:click={() => openChat(c.id)}>
          <div class="avatar">
            <Avatar id={c.peerId || c.id} name={c.name} src={c.avatar} size={38} />
            {#if c.kind === 'direct'}<span class="presence" class:on={c.online} />{:else}<span class="kind"><Icon name="hash" size={11} stroke={2.2} /></span>{/if}
          </div>
          <div class="body">
            <div class="row">
              <span class="grow name">{c.name}</span>
              {#if c.request}<span class="tag req" title={$t('chats.requestTitle')}>{$t('chats.requestTag')}</span>{/if}
              {#if c.blocked}<span class="tag blk" title={$t('chats.blockedTitle')}><Icon name="lock" size={11} /></span>{/if}
              {#if $activeGroupCalls[c.id]}<span class="oncall" title={$t('chats.callActive')}><Icon name="phone" size={13} /></span>{/if}
              {#if c.ts}<span class="time">{when(c.ts, $lang)}</span>{/if}
            </div>
            <div class="row">
              <span class="grow last">{c.last || (c.kind === 'group' ? $t('chats.members', { n: c.members }) : $t('chats.noMessages'))}</span>
              {#if c.pending}<span class="tag wait" title={$t('chats.pending.' + c.pending)}><Icon name="clock" size={11} /></span>{/if}
              {#if c.requests}<span class="tag reqs" title={$t('chats.req.title', { n: c.requests })}><Icon name="userPlus" size={11} /> {c.requests}</span>{/if}
              {#if c.mentions}<span class="badge mention" title={$t('chats.mentionsTitle', { n: c.mentions })}>@</span>{/if}
              {#if c.unread}<span class="badge" title={$t('chats.unreadTitle', { n: c.unread })}>{c.unread}</span>{/if}
            </div>
          </div>
        </button>
      {/each}
      {:else}
        {#if !listLoaded && !q}
          {#each [0, 1, 2, 3, 4] as i (i)}
            <div class="skel"><Skeleton circle height={38} /><div class="lines"><Skeleton width="55%" height={12} /><Skeleton width="80%" height={10} /></div></div>
          {/each}
        {:else}
        <div class="empty">
          {#if q}
            {#if !savedRow}<p class="small muted">{$t('chats.nothingFound', { query })}</p>{/if}
          {:else}
            <span class="empty-ic"><Icon name="message" size={22} /></span>
            <p class="small muted">{$t('chats.empty')}</p>
          {/if}
        </div>
        {/if}
      {/each}
    </div>
    <SelfCard />
    {#if !narrow}
      <!-- svelte-ignore a11y-no-static-element-interactions -->
      <div class="resize" title={$t('chats.dragResize')} on:mousedown|preventDefault={startResize} on:dblclick={() => { listWidth = 320; endResize(); }}></div>
    {/if}
  </aside>
  </PanelHost>
  {/if}

  {#if !narrow || opened}
  <section>
    {#if showSaved}
      <Saved back={narrow} on:back={() => activeId.set('')} />
    {:else if active}
      <Conversation conv={active} back={narrow} stage={showCall} chatOpen={chatOpen} theatre={maximized} on:back={() => activeId.set('')} let:compact let:toggleChannels>
        {#if showCall}<CallStage title={stageTitle(active)} bind:maximized {chatOpen} unread={missed} channelsButton={compact && isSpace} on:channels={toggleChannels} on:togglechat={toggleCallChat} />{/if}
      </Conversation>
    {:else}
      <div class="placeholder">
        <span class="ph-ic" aria-hidden="true"><Icon name="message" size={26} /></span>
        <h3>{$t('chats.selectChat')}</h3>
        <p class="small muted">{$t('chats.e2e')}</p>
      </div>
    {/if}
  </section>
  {/if}
</div>

<style>
  .chats { display: flex; height: 100%; }
  .chats.resizing { user-select: none; }
  aside {
    position: relative; flex: none; display: flex; flex-direction: column; min-height: 0;
    border-right: 1px solid var(--border); background: var(--surface-1);
  }
  .narrow aside { width: 100%; border-right: none; }
  .head { display: flex; align-items: center; gap: 2px; padding: 12px 8px 8px 16px; min-height: 56px; }
  .head h2 { font-size: var(--fs-lg); line-height: var(--lh-lg); }

  .head .new { border: 1px solid var(--border); margin-right: 4px; width: var(--ctl-md); height: var(--ctl-md); }
  .head .icon { width: var(--ctl-md); height: var(--ctl-md); }
  .newmenu { display: flex; flex-direction: column; padding: 4px; gap: 1px; }
  .nm-item { display: flex; align-items: center; gap: 10px; width: 100%; padding: 7px 10px; border: none; border-radius: var(--r-md); background: transparent; text-align: left; font-weight: var(--fw-medium); font-size: var(--fs-md); }
  .nm-item:hover:not(:disabled) { background: var(--hover); border: none; }
  .section { display: flex; align-items: center; gap: 6px; padding: 12px 10px 4px; font-size: var(--fs-2xs); font-weight: var(--fw-bold); color: var(--faint); letter-spacing: 0.05em; text-transform: uppercase; }
  .section .cnt { font-weight: var(--fw-medium); letter-spacing: 0; }

  .sheet { padding: 0 12px 8px; overflow: hidden; }
  .sheet-in { padding: 12px; border-radius: var(--r-xl); background: var(--surface-2); border: 1px solid var(--border); display: flex; flex-direction: column; gap: 10px; }
  .sheet-in textarea, .sheet-in input { background: var(--surface-1); }
  .sheet-head { margin: -2px -4px -2px 2px; }
  .sheet-in .row { gap: 6px; }
  .sheet-in .row > button { min-width: 0; padding: 6px 8px; font-size: var(--fs-sm); overflow: hidden; text-overflow: ellipsis; }
  .icon.tiny { width: var(--ctl-xs); height: var(--ctl-xs); border-radius: var(--r-md); }
  .center { justify-content: center; }
  .qr { width: 176px; height: 176px; border-radius: var(--r-xl); align-self: center; padding: 8px; background: #fff; box-shadow: var(--shadow-sm); }

  .search {
    display: flex; align-items: center; gap: 8px; margin: 0 12px 6px; padding: 0 4px 0 10px; height: var(--ctl-md);
    border-radius: var(--r-md); background: var(--surface-2); color: var(--faint); border: 1px solid transparent;
    transition: border-color var(--dur), box-shadow var(--dur), background var(--dur), color var(--dur);
  }
  .search:focus-within { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); color: var(--accent-fg); }
  .search input { border: none; background: transparent; padding: 0; height: 100%; box-shadow: none; font-size: var(--fs-md); }
  .search input:focus { box-shadow: none; background: transparent; }

  .list { flex: 1; overflow-y: auto; padding: 0 6px 8px; }
  .conv {
    width: 100%; text-align: left; display: flex; gap: 10px; align-items: center; padding: 6px 8px; min-height: var(--row-h);
    border-radius: var(--r-lg); margin-bottom: 1px; background: transparent; border: none; font-weight: 400;
  }
  .conv:hover:not(:disabled) { background: var(--hover); border: none; }
  .conv.sel, .conv.sel:hover:not(:disabled) { background: var(--surface-3); }
  .avatar { position: relative; flex: none; }
  .presence { position: absolute; right: -2px; bottom: -2px; width: 11px; height: 11px; border-radius: 50%; background: var(--faint); border: 2px solid var(--surface-1); }
  .conv.sel .presence { border-color: var(--surface-3); }
  .presence.on { background: var(--ok); }
  .saved-av { width: 38px; height: 38px; border-radius: 50%; display: grid; place-items: center; color: var(--accent-fg); background: var(--accent-soft); }
  .avatar .kind { position: absolute; right: -4px; bottom: -4px; width: 17px; height: 17px; border-radius: var(--r-sm); background: var(--surface-3); border: 2px solid var(--surface-1); color: var(--muted); display: grid; place-items: center; }
  .conv.sel .avatar .kind { border-color: var(--surface-3); background: var(--surface-1); }
  .body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 1px; }
  .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: var(--fw-medium); font-size: var(--fs-md); color: var(--text); }
  .time { color: var(--faint); font-size: var(--fs-xs); font-variant-numeric: tabular-nums; }
  .oncall { display: inline-flex; color: var(--ok-fg); }
  .conv.unread .name { font-weight: var(--fw-semibold); }
  .conv.unread .time { color: var(--accent-fg); font-weight: var(--fw-semibold); }
  .last { color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: var(--fs-sm); }
  .conv.unread .last { color: var(--text); }
  .conv .badge { flex: none; min-width: 18px; height: 18px; padding: 0 5px; border-radius: var(--r-pill); background: var(--accent); color: var(--accent-ink); font-size: var(--fs-2xs); font-weight: var(--fw-bold); display: inline-grid; place-items: center; }
  .conv .badge.mention { background: var(--danger); color: var(--danger-ink); }
  .empty { padding: 28px 18px; display: flex; flex-direction: column; align-items: center; gap: 12px; text-align: center; }
  .empty-ic { width: 44px; height: 44px; border-radius: var(--r-xl); display: grid; place-items: center; background: var(--surface-2); color: var(--muted); }
  .skel { display: flex; align-items: center; gap: 10px; padding: 8px 8px; }
  .skel .lines { flex: 1; display: flex; flex-direction: column; gap: 6px; }

  .resize { position: absolute; top: 0; bottom: 0; right: -3px; width: 6px; cursor: col-resize; z-index: 3; }
  .resize::after { content: ''; position: absolute; top: 0; bottom: 0; left: 2px; width: 2px; background: var(--accent); opacity: 0; transition: opacity var(--dur); }
  .resize:hover::after, .resizing .resize::after { opacity: 0.6; }

  section { flex: 1; min-width: 0; display: flex; flex-direction: column; position: relative; background: var(--bg); }

  .placeholder { flex: 1; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; text-align: center; padding: 24px; }
  .ph-ic { width: 56px; height: 56px; border-radius: var(--r-xl); display: grid; place-items: center; background: var(--surface-2); color: var(--muted); margin-bottom: 8px; }
  .tag { flex: none; font-size: var(--fs-2xs); font-weight: var(--fw-bold); padding: 1px 6px; border-radius: var(--r-sm); display: inline-flex; align-items: center; }
  .tag.req { background: var(--accent-soft); color: var(--accent-fg); }
  .tag.blk { background: var(--danger-soft); color: var(--danger-fg); }
  .tag.wait { background: var(--warn-soft); color: var(--warn-fg); }
  .tag.reqs { background: var(--accent-soft); color: var(--accent-fg); gap: 3px; }
</style>

