<script>
  import { createEventDispatcher, onDestroy, onMount, tick } from 'svelte';
  import { derived, writable } from 'svelte/store';
  import { fly } from 'svelte/transition';
  import { App, errText, openURL } from '../api.js';
  import { classifyLink, fromWebLink, openLinkDialog } from '../links.js';
  import { messages, chatError, chatNote, activeGroupCalls, send, quotes, thread, fetchQuote, openThread, closeThread, editMessage, channels, activeChannel, mainChannel } from '../stores/chats.js';
  import { isCallActive, startCall, peerInfo } from '../stores/call.store.js';
  import { fileError, loadTransfers, pickAndSend, sendPaths } from '../stores/files.js';
  import { savedError } from '../stores/saved.js';
  import { nameOf } from '../stores/people.js';
  import { draftOf, keepDraft } from '../stores/drafts.js';
  import { t, intlLocale } from '../i18n/index.js';
  import Icon from '../components/ui/Icon.svelte';
  import ChatHeader from '../features/chat/ChatHeader.svelte';
  import FindBar from '../features/chat/FindBar.svelte';
  import PinBar from '../features/chat/PinBar.svelte';
  import MessageList from '../features/chat/MessageList.svelte';
  import Composer from '../features/chat/Composer.svelte';
  import ThreadPanel from '../features/chat/ThreadPanel.svelte';
  import MessageMenu from '../features/chat/MessageMenu.svelte';
  import RequestBar from '../features/chat/RequestBar.svelte';
  import ChatInfo from '../features/chat/ChatInfo.svelte';
  import ContactProfile from '../features/chat/ContactProfile.svelte';
  import ChannelBar from '../features/chat/ChannelBar.svelte';
  import ThreadsList from '../features/chat/ThreadsList.svelte';
  import { provideChat } from '../features/chat/context.js';
  import '../features/chat/chat.css';

  /** An open chat: it puts the parts of features/chat together and holds what they share. What I may
   *  do here and with each message comes from the core (conv.can, m.can). */
  export let conv;
  /** back: show a back arrow (narrow window, the list is hidden); indent: leave room for the show-list button */
  export let back = false;
  export let indent = false;

  const dispatch = createEventDispatcher();
  let list;
  let composer;
  let threadPanel;

  // ---- what the parts share ----------------------------------------------------------------------
  const convStore = writable(conv);
  $: convStore.set(conv);
  const byId = derived(messages, ($m) => Object.fromEntries($m.map((m) => [m.id, m])));
  /** A message by id: loaded, or fetched as a quote (null while it is not there). */
  const quoteOf = derived([byId, quotes], ([$byId, $quotes]) => (id) => $byId[id] || $quotes[id] || null);
  /** Who wrote a message, as the screen names them. */
  const authorName = derived([nameOf, convStore, t], ([$nameOf, $conv, $t]) => (m) =>
    m.mine ? $t('chats.You') : $conv.kind === 'group' ? $nameOf(m.sender) : $conv.name);

  provideChat({
    nameOf, quoteOf, authorName,
    goTo: (id) => list?.goTo(id),
    openMenu, startReply, startEdit, showThread,
    fullTime: (ts) => new Date(ts || 0).toLocaleString(intlLocale(), { weekday: 'short', day: 'numeric', month: 'long', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }),
    // a link opens in the system browser, never in this window; an EchoFog link (its https form
    // too) opens here, in the open-link dialog
    openLink: (e, href) => { e.preventDefault(); e.stopPropagation(); if (classifyLink(href)) openLinkDialog(fromWebLink(href)); else openURL(href); },
    dropDraftOf: (id) => { if (editing && editing.id === id) cancelCompose(); },
  });

  // an answer to a message not loaded: fetch the quoted one
  $: $messages.forEach((m) => { if (m.replyTo && !$byId[m.replyTo]) fetchQuote(m.replyTo); });

  // ---- switching chats: panels close, the draft of each chat is kept ------------------------------
  let shownId = '';
  let shownChannel = '';
  let info = false;
  let threadsOpen = false; // a space's list of threads
  let menu = null; // { m, anchor }
  let text = '';
  let replyTo = null;
  let editing = null;
  let sending = false;
  $: if (conv.id !== shownId) switchTo(conv.id);
  // the draft's key: the chat, and in a space the channel shown
  const draftKey = () => shownId + (shownChannel ? '/' + shownChannel : '');
  function switchTo(id) {
    keepDraft(draftKey(), { text, replyTo, editing });
    shownId = id;
    shownChannel = '';
    ({ text, replyTo, editing } = draftOf(id));
    info = false;
    menu = null;
    closeSearch();
  }
  onDestroy(() => keepDraft(draftKey(), { text, replyTo, editing }));
  $: loadTransfers(conv.kind === 'direct' ? conv.peerId : '');

  // ---- composing ---------------------------------------------------------------------------------
  async function submit() {
    if (!text.trim() || sending) return;
    sending = true;
    let ok;
    if (editing) {
      ok = editing.text === text.trim() || (await editMessage(editing.id, text));
      if (ok) editing = null;
    } else {
      ok = await send(text, { replyTo: replyTo ? replyTo.id : '' });
      if (ok) replyTo = null;
    }
    if (ok) text = '';
    sending = false;
  }

  function startReply(m) {
    menu = null;
    editing = null;
    replyTo = m;
    composer?.focus();
  }

  function startEdit(m) {
    menu = null;
    if (m.thread) {
      threadPanel?.edit(m);
      return;
    }
    replyTo = null;
    editing = m;
    text = m.text;
    composer?.focus();
  }

  function cancelCompose() {
    if (editing) text = '';
    editing = null;
    replyTo = null;
  }

  function editLast(e) {
    const mine = [...$messages].reverse().find((m) => m.can.edit);
    if (mine) {
      e.detail.preventDefault();
      startEdit(mine);
    }
  }

  async function showThread(m) {
    menu = null;
    info = false;
    await openThread(m.id);
  }

  function openMenu(e, m) {
    if (m.system) return;
    if (e.type === 'contextmenu' && e.target.closest('a')) return; // a link keeps its own menu
    e.preventDefault();
    menu = { m, anchor: e.currentTarget.closest('.msg, .comment') || e.currentTarget };
  }

  function toggleThreads() {
    threadsOpen = !threadsOpen;
    if (threadsOpen) { info = false; closeThread(); }
  }

  function toggleInfo() {
    info = !info;
    if (info) threadsOpen = false;
    if (info) closeThread();
  }

  async function call(video) {
    if (conv.kind === 'direct') {
      startCall({ alias: conv.name, peerId: conv.peerId, pubKey: conv.pubKey, chatId: conv.id }, video);
      return;
    }
    try {
      // a call already going on in the group is joined; only without one a new call starts (and rings)
      let c = $activeGroupCalls[conv.id] || (await App.ActiveGroupCall(conv.groupId));
      if (!c || !c.callId) c = await App.StartGroupCall(conv.groupId);
      startCall({ alias: conv.name, callId: c.callId, coordinator: c.coordinator, chatId: conv.id }, video);
    } catch (err) {
      chatError.set(errText(err, $t('chats.err.call')));
    }
  }
  // a space's voice room: its call is joined (or started, without ringing anybody)
  async function joinRoom(room) {
    try {
      const c = await App.StartRoomCall(conv.groupId, room.id);
      startCall({ alias: `${conv.name} · ${room.name}`, callId: c.callId, coordinator: c.coordinator, chatId: conv.id, room: room.id }, false);
    } catch (err) {
      chatError.set(errText(err, $t('chats.err.call')));
    }
  }
  $: shownChan = $activeChannel ? $channels.find((c) => c.id === $activeChannel) || null : null;
  $: channelName = shownChan ? shownChan.name : '';
  $: hasRooms = $channels.some((c) => c.voice);
  // in a channel only managers post to, the others comment; a channel's overrides can also let a
  // role post there that may not elsewhere, or keep it from posting
  $: mutedUntil = conv.can?.mutedUntil || 0;
  $: canPost = shownChan ? shownChan.canSend !== false : conv.kind === 'group' && $mainChannel.canSend !== undefined ? $mainChannel.canSend : conv.can.send;
  $: if ($thread) threadsOpen = false;
  // a draft per channel: switching channels keeps what was being written in each
  $: if ($activeChannel !== shownChannel) {
    keepDraft(draftKey(), { text, replyTo, editing });
    shownChannel = $activeChannel;
    ({ text, replyTo, editing } = draftOf(draftKey()));
  }
  $: groupCall = conv.kind === 'group' ? $activeGroupCalls[conv.id] : null;
  $: inThisCall = $isCallActive && $peerInfo.chatId === conv.id;

  // ---- search inside the chat --------------------------------------------------------------------
  let searching = false;
  let query = '';
  let findBar;
  let cur = -1; // index into matches (oldest first); a new search starts at the newest
  let lastQ = '';
  $: q = searching ? query.trim().toLowerCase() : '';
  const haystack = (m) => ((m.file ? m.file.name + ' ' : '') + (m.text || '')).toLowerCase();
  $: matches = q ? $messages.filter((m) => !m.system && haystack(m).includes(q)).map((m) => m.id) : [];
  $: if (q !== lastQ) {
    lastQ = q;
    cur = matches.length - 1;
    if (cur >= 0) list?.reveal(matches[cur], false);
  }
  $: currentMatch = cur >= 0 ? matches[cur] : '';
  async function openSearch() {
    if (searching) { findBar?.focus(); return; }
    searching = true;
    await tick();
  }
  function closeSearch() {
    searching = false;
    query = '';
    cur = -1;
  }
  function step(dir) {
    if (!matches.length) return;
    cur = (cur + dir + matches.length) % matches.length;
    list?.reveal(matches[cur], false);
  }
  function windowKey(e) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'f') { e.preventDefault(); openSearch(); }
  }

  // ---- files -------------------------------------------------------------------------------------
  onMount(() => {
    // files dropped on the chat are offered to the contact or posted to the group
    window.runtime?.OnFileDrop?.((_x, _y, paths) => {
      if (conv.can.files && paths?.length) sendPaths(conv, paths, $activeChannel);
    }, true);
  });
  onDestroy(() => window.runtime?.OnFileDropOff?.());
</script>

<svelte:window on:keydown={windowKey} />

<div class="chat-screen">
  {#if conv.kind === 'group'}
    <ChannelBar {conv} on:room={(e) => joinRoom(e.detail)} on:settings={() => { info = true; threadsOpen = false; }} />
  {/if}
  <div class="main">
    <ChatHeader {conv} {back} {indent} {searching} {info} {threadsOpen} {hasRooms} channel={shownChan} on:back={() => dispatch('back')} on:info={toggleInfo} on:threads={toggleThreads}
      on:search={() => (searching ? closeSearch() : openSearch())} on:call={(e) => call(e.detail)} />

    {#if searching}
      <FindBar bind:this={findBar} bind:query count={matches.length} index={cur} active={!!q} on:step={(e) => step(e.detail)} on:close={closeSearch} />
    {/if}

    <PinBar chatId={conv.id} canPin={conv.can.pin} />

    {#if conv.kind === 'group' && conv.pending}
      <div class="small upg pending"><Icon name="clock" size={14} /> <span>{$t('chats.pending.' + conv.pending)}</span></div>
    {:else if conv.kind === 'group' && !conv.upgraded}
      <p class="small upg">{$t('chats.groupNotUpgraded')}</p>
    {/if}
    {#if conv.requests}
      <button class="small upg reqs-bar" on:click={() => (info = true)}><Icon name="userPlus" size={14} /> {$t('chats.req.waiting', { n: conv.requests })}</button>
    {/if}

    <MessageList bind:this={list} {conv} {q} {matches} {currentMatch} />

    {#if groupCall && !inThisCall}
      <div class="callbar" transition:fly={{ y: -6, duration: 200 }}>
        <span class="pulse"></span>
        <span class="small">{$t('chats.callActive')}</span>
        <button class="primary small with-icon" on:click={() => call(false)} disabled={$isCallActive}><Icon name="phone" size={14} /> {$t('chats.joinCall')}</button>
      </div>
    {/if}

    {#if $chatError}<p class="error small bar" transition:fly={{ y: 6, duration: 200 }}>{$chatError}</p>{/if}
    {#if $chatNote}<p class="muted small bar" transition:fly={{ y: 6, duration: 200 }}>{$chatNote}</p>{/if}
    {#if $fileError}<p class="error small bar" transition:fly={{ y: 6, duration: 200 }}>{$fileError}</p>{/if}
    {#if $savedError}<p class="error small bar" transition:fly={{ y: 6, duration: 200 }}>{$savedError}</p>{/if}

    {#if conv.kind === 'direct'}<RequestBar {conv} />{/if}

    {#if conv.kind === 'direct' && conv.blocked}
      <!-- no composer: a blocked person gets nothing either way -->
    {:else if conv.kind === 'group' && conv.pending}
      <p class="small muted bar ro"><Icon name="clock" size={14} /> {$t('chats.pending.short')}</p>
    {:else if mutedUntil * 1000 > Date.now()}
      <p class="small muted bar ro"><Icon name="clock" size={14} /> {$t('chats.to.mine', { when: new Date(mutedUntil * 1000).toLocaleString(intlLocale(), { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' }) })}</p>
    {:else if !canPost}
      <p class="small muted bar ro"><Icon name="lock" size={14} /> {conv.can.send ? $t('chats.readOnlyChannelNote') : $t('chats.readOnly')}</p>
    {:else}
      <Composer bind:this={composer} bind:text {replyTo} {editing} replyName={replyTo ? $authorName(replyTo) : ''}
        files={conv.can.files} busy={sending}
        placeholder={conv.kind === 'group' ? $t('chats.writeIn', { name: channelName || $t('chats.mainChannel') }) : $t('chats.writeMessage')}
        on:submit={submit} on:cancel={cancelCompose} on:editlast={editLast} on:attach={() => pickAndSend(conv, $activeChannel)} on:goto={(e) => list?.goTo(e.detail)} />
    {/if}
  </div>

  {#if info}
    {#if conv.kind === 'direct'}
      <ContactProfile {conv} on:close={toggleInfo} on:call={(e) => call(e.detail)} />
    {:else}
      <ChatInfo {conv} on:close={toggleInfo} />
    {/if}
  {/if}

  {#if threadsOpen && !info && !$thread}
    <ThreadsList on:close={() => (threadsOpen = false)} />
  {/if}

  {#if $thread && !info}
    <ThreadPanel bind:this={threadPanel} {conv} />
  {/if}

  {#if menu}
    <MessageMenu {conv} m={menu.m} anchor={menu.anchor} on:close={() => (menu = null)} />
  {/if}
</div>
