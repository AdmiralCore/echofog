<script>
  import { onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import QRCode from 'qrcode';
  import { me, profile } from '../stores/session.js';
  import { chats, activeId, activeGroupCalls, loadChats, openChat, addContact, createGroup, joinGroup } from '../stores/chats.js';
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

  // the chat list can be hidden so the conversation uses the full width; the choice is remembered
  let listCollapsed = false;
  try { listCollapsed = localStorage.getItem('echofog.chatsCollapsed') === '1'; } catch (_) {}
  function toggleList() {
    listCollapsed = !listCollapsed;
    try { localStorage.setItem('echofog.chatsCollapsed', listCollapsed ? '1' : '0'); } catch (_) {}
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

  let query = '';
  $: q = query.trim().toLowerCase();
  $: sorted = [...$chats]
    .filter((c) => !q || (c.name || '').toLowerCase().includes(q) || (c.last || '').toLowerCase().includes(q))
    .sort((a, b) => (b.ts || 0) - (a.ts || 0));
  $: active = $chats.find((c) => c.id === $activeId);
  $: showSaved = $activeId === SAVED_ID;
  $: opened = !!active || showSaved;
  $: savedRow = !q || $t('saved.title').toLowerCase().includes(q);
  $: showCall = $isCallActive && !!active && $peerInfo.chatId === $activeId && $stageExpanded;
  let maximized = false;
  $: if (!showCall) maximized = false;
  $: parsed = parseContactInput(contactInput);
  $: myLink = $me.pubKey ? makeContactLink(currentDisplayName($profile), $me.pubKey) : '';

  onMount(() => {
    loadChats();
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

<svelte:window on:mousemove={onResize} on:mouseup={endResize} />

<div class="chats" class:narrow bind:clientWidth={width} class:resizing={!!resizing}>
  {#if (!listCollapsed || narrow) && (!narrow || !opened)}
  <aside style={narrow ? '' : `width:${listWidth}px`}>
    <div class="head">
      <h2 class="grow">{$t('chats.title')}</h2>
      <button class="icon" class:on={panel === 'me'} title={$t('chats.myContact')} aria-label={$t('chats.myContact')} on:click={() => toggle('me')}><Icon name="qr" size={18} /></button>
      {#if !narrow}<button class="icon" title={$t('chats.hideList')} aria-label={$t('chats.hideList')} on:click={toggleList}><Icon name="panel" size={18} /></button>{/if}
    </div>

    <div class="actions">
      <button class="act" class:on={panel === 'contact'} on:click={() => toggle('contact')}><Icon name="userPlus" size={16} /><span>{$t('chats.contact')}</span></button>
      <button class="act" class:on={panel === 'group'} on:click={() => toggle('group')}><Icon name="users" size={16} /><span>{$t('chats.group')}</span></button>
      <button class="act" class:on={panel === 'join'} on:click={() => toggle('join')}><Icon name="link" size={16} /><span>{$t('common.join')}</span></button>
    </div>

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
          <div class="avatar"><span class="saved-av"><Icon name="bookmark" size={20} /></span></div>
          <div class="body">
            <div class="row"><span class="grow name">{$t('saved.title')}</span></div>
            <div class="row"><span class="grow last">{$saved.length ? $t('saved.count', { n: $saved.length }) : $t('saved.hint')}</span></div>
          </div>
        </button>
      {/if}
      {#each sorted as c (c.id)}
        <button class="conv" class:sel={c.id === $activeId} class:unread={c.unread} on:click={() => openChat(c.id)}>
          <div class="avatar">
            <Avatar id={c.peerId || c.id} name={c.name} size={44} />
            {#if c.kind === 'direct'}<span class="presence" class:on={c.online} />{:else}<span class="kind">#</span>{/if}
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
              {#if c.unread}<span class="badge">{c.unread}</span>{/if}
            </div>
          </div>
        </button>
      {:else}
        <div class="empty">
          {#if q}
            {#if !savedRow}<p class="small muted">{$t('chats.nothingFound', { query })}</p>{/if}
          {:else}
            <span class="empty-ic"><Icon name="message" size={22} /></span>
            <p class="small muted">{$t('chats.empty')}</p>
          {/if}
        </div>
      {/each}
    </div>
    {#if !narrow}
      <!-- svelte-ignore a11y-no-static-element-interactions -->
      <div class="resize" title={$t('chats.dragResize')} on:mousedown|preventDefault={startResize} on:dblclick={() => { listWidth = 320; endResize(); }}></div>
    {/if}
  </aside>
  {/if}

  {#if !narrow || opened}
  <section>
    {#if listCollapsed && !narrow}
      <button class="show-list" title={$t('chats.showList')} aria-label={$t('chats.showList')} on:click={toggleList}><Icon name="panel" size={17} /></button>
    {/if}
    {#if showCall}
      <div class="call" class:full={maximized}><CallStage title={active.name} bind:maximized /></div>
    {/if}
    {#if showSaved}
      <Saved back={narrow} indent={listCollapsed && !narrow} on:back={() => activeId.set('')} />
    {:else if active}
      {#if !maximized}<Conversation conv={active} back={narrow} indent={listCollapsed && !narrow} on:back={() => activeId.set('')} />{/if}
    {:else}
      <div class="placeholder">
        <div class="ph-art" aria-hidden="true">
          <span class="b b1"></span><span class="b b2"></span><span class="b b3"></span>
        </div>
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
    border-right: 1px solid var(--line-soft); background: var(--panel);
    animation: slide-in-left 0.32s var(--ease);
  }
  .narrow aside { width: 100%; border-right: none; }
  @keyframes slide-in-left { from { opacity: 0; transform: translateX(-10px); } to { opacity: 1; transform: none; } }
  .head { display: flex; align-items: center; gap: 2px; padding: 16px 12px 8px 20px; }
  .head h2 { font-size: 20px; }

  .actions { display: flex; gap: 6px; padding: 4px 14px 10px; }
  .act {
    flex: 1; display: flex; align-items: center; justify-content: center; gap: 7px; padding: 7px 8px;
    font-size: 12.5px; color: var(--muted); background: var(--raised); border-color: transparent; border-radius: 11px;
  }
  .act:hover:not(:disabled) { color: var(--text); border-color: transparent; }
  .act.on, .act.on:hover:not(:disabled) { color: var(--accent); background: var(--accent-soft); }

  .sheet { padding: 0 14px 10px; overflow: hidden; }
  .sheet-in { padding: 12px; border-radius: 16px; background: var(--raised); border: 1px solid var(--line-soft); display: flex; flex-direction: column; gap: 10px; }
  .sheet-in textarea, .sheet-in input { background: var(--panel); }
  .sheet-head { margin: -2px -4px -2px 2px; }
  .icon.tiny { width: 24px; height: 24px; border-radius: 7px; }
  .kind { align-self: stretch; }
  .center { justify-content: center; }
  .qr { width: 176px; height: 176px; border-radius: 16px; align-self: center; padding: 8px; background: #fff; box-shadow: var(--shadow); animation: pop 0.4s var(--spring); }

  .search {
    display: flex; align-items: center; gap: 8px; margin: 0 14px 8px; padding: 0 6px 0 12px; height: 38px;
    border-radius: 12px; background: var(--raised); color: var(--faint); border: 1px solid transparent;
    transition: border-color var(--dur), box-shadow var(--dur), background var(--dur), color var(--dur);
  }
  .search:focus-within { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); background: var(--panel); color: var(--accent); }
  .search input { border: none; background: transparent; padding: 0; height: 100%; box-shadow: none; }
  .search input:focus { box-shadow: none; background: transparent; }

  .list { flex: 1; overflow-y: auto; padding: 2px 8px 10px; }
  .conv {
    width: 100%; text-align: left; display: flex; gap: 12px; align-items: center; padding: 9px 10px;
    border-radius: 14px; margin-bottom: 2px; background: transparent; border: none; font-weight: 400;
    animation: fade-up 0.3s var(--ease) both;
  }
  .conv:hover:not(:disabled) { background: var(--hover); border: none; }
  .conv:active:not(:disabled) { transform: scale(0.985); }
  .conv.sel, .conv.sel:hover:not(:disabled) { background: var(--accent-soft); }
  .avatar { position: relative; flex: none; }
  .presence { position: absolute; right: -1px; bottom: -1px; width: 13px; height: 13px; border-radius: 50%; background: var(--faint); border: 2.5px solid var(--panel); transition: background 0.3s; }
  .presence.on { background: var(--ok); }
  .saved-av { width: 44px; height: 44px; border-radius: 50%; display: grid; place-items: center; color: #fff; background: linear-gradient(135deg, var(--accent), var(--accent-2)); }
  .avatar .kind { position: absolute; right: -3px; bottom: -3px; width: 18px; height: 18px; border-radius: 6px; background: var(--raised); border: 2px solid var(--panel); color: var(--muted); font-size: 10px; font-weight: 750; display: grid; place-items: center; }
  .body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 1px; }
  .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 620; color: var(--text); }
  .time { color: var(--faint); font-size: 11.5px; font-variant-numeric: tabular-nums; }
  .oncall { display: inline-flex; color: var(--ok); }
  .conv.unread .time { color: var(--accent); font-weight: 600; }
  .last { color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }
  .conv.unread .last { color: var(--text); }
  .empty { padding: 28px 18px; display: flex; flex-direction: column; align-items: center; gap: 12px; text-align: center; }
  .empty-ic { width: 48px; height: 48px; border-radius: 16px; display: grid; place-items: center; background: var(--accent-soft); color: var(--accent); }

  .resize { position: absolute; top: 0; bottom: 0; right: -3px; width: 6px; cursor: col-resize; z-index: 3; }
  .resize::after { content: ''; position: absolute; top: 0; bottom: 0; left: 2px; width: 2px; background: var(--accent); opacity: 0; transition: opacity 0.2s; }
  .resize:hover::after, .resizing .resize::after { opacity: 0.6; }

  section { flex: 1; min-width: 0; display: flex; flex-direction: column; position: relative; background: var(--bg); }
  .narrow section { animation: slide-in-right 0.3s var(--ease); }
  .show-list {
    position: absolute; z-index: 5; top: 14px; left: 14px;
    width: 34px; height: 34px; padding: 0; border-radius: 11px; display: grid; place-items: center;
    background: var(--panel); border: 1px solid var(--line-soft); color: var(--muted); box-shadow: var(--shadow-sm);
  }
  .show-list:hover:not(:disabled) { color: var(--text); background: var(--panel); }
  .call { height: 58%; min-height: 300px; flex: none; background: #000; transition: height 0.35s var(--ease); }
  .call.full { height: 100%; flex: 1; }

  .placeholder { flex: 1; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; text-align: center; padding: 24px; animation: fade 0.5s ease-out; }
  .ph-art { position: relative; width: 120px; height: 90px; margin-bottom: 14px; }
  .b { position: absolute; border-radius: 18px; animation: float 5s ease-in-out infinite; }
  .b1 { left: 0; top: 10px; width: 72px; height: 34px; background: var(--raised); border: 1px solid var(--line-soft); border-bottom-left-radius: 6px; }
  .b2 { right: 0; top: 34px; width: 80px; height: 34px; background: linear-gradient(135deg, var(--accent), var(--accent-2)); border-bottom-right-radius: 6px; animation-delay: -1.6s; box-shadow: var(--glow); }
  .b3 { left: 14px; top: 62px; width: 46px; height: 24px; background: var(--raised); border: 1px solid var(--line-soft); border-bottom-left-radius: 6px; animation-delay: -3.2s; }
  @keyframes float { 0%, 100% { transform: translateY(0); } 50% { transform: translateY(-6px); } }
  .tag { flex: none; font-size: 10.5px; font-weight: 700; padding: 1px 6px; border-radius: 6px; display: inline-flex; align-items: center; }
  .tag.req { background: var(--accent-soft, rgba(88,101,242,.18)); color: var(--accent, #5865f2); }
  .tag.blk { background: var(--danger-soft, rgba(242,63,67,.15)); color: var(--danger, #f23f43); }
</style>

