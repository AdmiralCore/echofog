<script>
  import { afterUpdate, createEventDispatcher, onDestroy, onMount, tick } from 'svelte';
  import { fly, slide } from 'svelte/transition';
  import { App, errText, copy, openURL } from '../api.js';
  import {
    chats, messages, chatError, chatNote, membersVersion, activeId, activeGroupCalls, loadChats, send, deleteContact, leaveGroup, acceptRequest, dismissRequest, blockPeer, unblockPeer,
    hasOlder, pins, thread, quotes, loadOlder, fetchQuote, openThread, closeThread, editMessage, deleteMessage, react, pinMessage, markRead, preview,
  } from '../stores/chats.js';
  import { isCallActive, startCall, stageExpanded, peerInfo } from '../stores/call.store.js';
  import { transfers, fileError, loadTransfers, pickAndSend, sendPaths, openFile, showInFolder } from '../stores/files.js';
  import { savedIds, savedError, toggleSaved, saveAsFile, jumpTo, SAVED_ID } from '../stores/saved.js';
  import { time, short, bytes } from '../format.js';
  import { linkify, mark, linkHost } from '../linkify.js';
  import { t, lang, intlLocale } from '../i18n/index.js';
  import { ask } from '../stores/dialog.js';
  import { me } from '../stores/session.js';
  import Avatar from '../components/ui/Avatar.svelte';
  import Icon from '../components/ui/Icon.svelte';
  import Popover from '../components/ui/Popover.svelte';
  import FileCard from '../components/FileCard.svelte';

  export let conv;
  /** back: show a back arrow (narrow window, the list is hidden); indent: leave room for the show-list button */
  export let back = false;
  export let indent = false;

  const dispatch = createEventDispatcher();
  let input;

  const RANK = { owner: 100, admin: 50, moderator: 25, member: 10, subscriber: 10, restricted: 1 };
  const ASSIGNABLE = ['admin', 'moderator', 'member', 'restricted'];

  $: groupCall = conv.kind === 'group' ? $activeGroupCalls[conv.id] : null;
  $: inThisCall = $isCallActive && $peerInfo.chatId === conv.id;

  let text = '';
  let sending = false;
  let scroller;
  let info = false;
  let members = [];
  let invite = '';
  let infoError = '';
  let copied = '';
  let newMemberKey = '';
  let busyMember = '';

  let shownId = '';
  // another chat closes the panels and the draft's answer; a refresh of this one does not
  $: if (conv.id !== shownId) {
    shownId = conv.id;
    info = false;
    closeSearch();
    menu = null;
    replyTo = null;
    editing = null;
    newestId = '';
    lastRead = '';
    pinIdx = 0;
    atBottom = true;
  }
  $: loadTransfers(conv.kind === 'direct' ? conv.peerId : '');
  $: myRank = RANK[conv.myRole] || 0;
  $: canManage = conv.kind === 'group' && myRank >= RANK.admin;
  $: if (info && conv.kind === 'group') { void $membersVersion; loadMembers(); }

  // ---- what the log lets me do here ------------------------------------------------------------
  // A group whose members have not all updated runs the old way: plain messages only.
  $: isGroup = conv.kind === 'group';
  $: logOps = !isGroup || conv.upgraded !== false; // null: not known yet (nothing written): let the core decide
  $: canWrite = !conv.readOnly && !(conv.kind === 'direct' && conv.blocked);
  $: canReply = logOps && canWrite;
  $: canReact = logOps && !conv.readOnly; // the readers of a channel cannot react (yet)
  $: canPin = logOps && (!isGroup || myRank >= RANK.moderator);
  $: canEdit = (m) => logOps && m.mine && !m.deleted && !m.file && m.type === 0;
  $: canDelete = (m) => logOps && !m.deleted && (m.mine || (isGroup && myRank >= RANK.moderator));
  const QUICK = ['👍', '❤️', '😂', '😮', '😢', '🔥'];
  $: myId = $me.id;

  $: byId = Object.fromEntries($messages.map((m) => [m.id, m]));
  $: quoteOf = (id) => byId[id] || $quotes[id] || null;
  // an answer to a message not loaded: fetch the quoted one
  $: $messages.forEach((m) => { if (m.replyTo && !byId[m.replyTo]) fetchQuote(m.replyTo); });

  // ---- scrolling ---------------------------------------------------------------------------------
  // The end of the chat stays in view while I am there; scrolling to the top loads older messages
  // without moving what is on screen.
  let newestId = '';
  let atBottom = true;
  let prepending = null; // { height, top, firstId } before an older page is put in front

  afterUpdate(() => {
    if (pendingJump && $messages.some((m) => m.id === pendingJump)) {
      const id = pendingJump;
      pendingJump = '';
      newestId = $messages[$messages.length - 1].id;
      reveal(id);
      return;
    }
    if (!scroller) return;
    if (prepending && $messages.length && $messages[0].id !== prepending.firstId) {
      scroller.scrollTop = scroller.scrollHeight - prepending.height + prepending.top;
      prepending = null;
      return;
    }
    const last = $messages[$messages.length - 1];
    const id = last ? last.id : '';
    if (id !== newestId) {
      // opening a chat jumps straight to the end; a new message glides into view
      const first = !newestId;
      newestId = id;
      if (first || atBottom || (last && last.mine)) {
        scroller.scrollTo({ top: scroller.scrollHeight, behavior: first ? 'auto' : 'smooth' });
        atBottom = true;
        readSoon();
      }
    }
    // a first page that does not fill the window: fetch more until it does
    if (!prepending && $hasOlder && scroller.scrollHeight <= scroller.clientHeight + 40) older();
  });

  function onScroll() {
    atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80;
    if (scroller.scrollTop < 160 && $hasOlder && !prepending) older();
    if (atBottom) readSoon();
  }

  async function older() {
    prepending = { height: scroller.scrollHeight, top: scroller.scrollTop, firstId: $messages[0]?.id || '' };
    if (!(await loadOlder())) prepending = null;
  }

  // ---- read -------------------------------------------------------------------------------------
  // The chat counts as read up to its newest message once that is on screen in a window in front.
  let lastRead = '';
  let readTimer;
  function readSoon() {
    clearTimeout(readTimer);
    readTimer = setTimeout(() => {
      const last = $messages[$messages.length - 1];
      if (!last || !atBottom || !document.hasFocus()) return;
      if (last.id === lastRead && !conv.unread) return;
      lastRead = last.id;
      markRead(last.id);
    }, 1000);
  }

  // ---- composing --------------------------------------------------------------------------------
  let replyTo = null; // the message answered
  let editing = null; // my message whose text is being changed

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
    await tick();
    grow();
  }

  async function startReply(m) {
    menu = null;
    editing = null;
    replyTo = m;
    await tick();
    input?.focus();
  }

  async function startEdit(m) {
    menu = null;
    replyTo = null;
    editing = m;
    text = m.text;
    await tick();
    grow();
    input?.focus();
  }

  function cancelCompose() {
    if (editing) text = '';
    editing = null;
    replyTo = null;
    tick().then(grow);
  }

  // the composer grows with its text up to a few lines
  function grow() {
    if (!input) return;
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 160) + 'px';
  }

  // consecutive messages from the same person within a few minutes form one visual group
  const GROUP_GAP = 5 * 60 * 1000;
  const sameGroup = (a, b) => a && b && !a.system && !b.system && a.mine === b.mine && a.sender === b.sender && Math.abs((b.ts || 0) - (a.ts || 0)) < GROUP_GAP;

  function keydown(e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
    } else if (e.key === 'Escape' && (editing || replyTo)) {
      e.preventDefault();
      cancelCompose();
    } else if (e.key === 'ArrowUp' && !text) {
      // the last message of mine opens for editing, as in other messengers
      const mine = [...$messages].reverse().find((m) => canEdit(m));
      if (mine) { e.preventDefault(); startEdit(mine); }
    }
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

  async function loadMembers() {
    try {
      members = (await App.ListGroupMembers(conv.groupId)) || [];
      invite = await App.GetGroupInvite(conv.groupId);
      infoError = '';
    } catch (err) {
      infoError = errText(err);
    }
  }

  function toggleInfo() {
    info = !info;
    infoError = '';
    if (info) closeThread();
  }

  // ---- threads (comments under a message) ------------------------------------------------------
  let threadText = '';
  let threadSending = false;
  let threadInput;
  $: threadRoot = $thread ? quoteOf($thread.root) : null;
  $: if ($thread && !threadRoot) fetchQuote($thread.root);

  async function showThread(m) {
    menu = null;
    info = false;
    await openThread(m.id);
    await tick();
    threadInput?.focus();
  }
  let editingComment = null;
  async function editComment(m) {
    menu = null;
    editingComment = m;
    threadText = m.text;
    await tick();
    threadInput?.focus();
  }
  async function sendComment() {
    if (!threadText.trim() || threadSending || !$thread) return;
    threadSending = true;
    const ok = editingComment
      ? editingComment.text === threadText.trim() || (await editMessage(editingComment.id, threadText))
      : await send(threadText, { threadRoot: $thread.root });
    if (ok) { threadText = ''; editingComment = null; }
    threadSending = false;
  }
  function threadKey(e) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendComment(); }
    else if (e.key === 'Escape' && editingComment) { e.preventDefault(); editingComment = null; threadText = ''; }
    else if (e.key === 'Escape') { e.preventDefault(); closeThread(); }
  }
  $: if (!$thread) editingComment = null;

  // ---- pins ---------------------------------------------------------------------------------------
  // The bar shows the newest pin; a click goes to it and moves on to the one pinned before it.
  let pinIdx = 0;
  $: if (pinIdx >= $pins.length) pinIdx = 0;
  $: shownPin = $pins[pinIdx] || null;
  function nextPin() {
    if (!shownPin) return;
    goTo(shownPin.id);
    pinIdx = (pinIdx + 1) % $pins.length;
  }

  // scrolls to a message, loading older pages until it is there (a pin or a quote far back)
  async function goTo(id) {
    for (let i = 0; i < 40 && !$messages.some((m) => m.id === id); i++) {
      if (!$hasOlder) break;
      prepending = { height: scroller.scrollHeight, top: scroller.scrollTop, firstId: $messages[0]?.id || '' };
      if (!(await loadOlder())) { prepending = null; break; }
      await tick();
    }
    reveal(id);
  }

  async function doCopy(what, value) {
    if (await copy(value)) {
      copied = what;
      setTimeout(() => (copied = ''), 1500);
    }
  }

  let reqName = '';
  let reqBusy = false;
  async function reqAction(fn) {
    reqBusy = true;
    infoError = '';
    try { await fn(); } catch (err) { infoError = errText(err); } finally { reqBusy = false; }
  }
  const accept = () => reqAction(() => acceptRequest(conv, reqName));
  const dismiss = () => reqAction(() => dismissRequest(conv));
  async function block() {
    if (!(await ask({ message: $t('chats.confirm.block', { name: conv.name }), confirm: $t('chats.block'), danger: true }))) return;
    await reqAction(() => blockPeer(conv));
  }
  const unblock = () => reqAction(() => unblockPeer(conv));

  async function removeContact() {
    if (!(await ask({ message: $t('chats.confirm.deleteContact', { name: conv.name }), confirm: $t('common.delete'), danger: true }))) return;
    try { await deleteContact(conv); } catch (err) { infoError = errText(err); }
  }

  async function memberAction(id, fn) {
    busyMember = id;
    infoError = '';
    try {
      await fn();
      await loadMembers();
    } catch (err) {
      infoError = errText(err);
    } finally {
      busyMember = '';
    }
  }

  async function makeOwner(m) {
    if (!(await ask({ message: $t('chats.confirm.makeOwner', { name: senderName(m.peerId), group: conv.name }) }))) return;
    memberAction(m.peerId, async () => { await App.TransferGroupOwnership(conv.groupId, m.peerId); await loadChats(); });
  }

  async function deleteGroup() {
    if (!(await ask({ message: $t('chats.confirm.deleteGroup', { name: conv.name }), confirm: $t('common.delete'), danger: true }))) return;
    try { await App.DeleteGroup(conv.groupId); await loadChats(); activeId.set(''); } catch (err) { infoError = errText(err); }
  }

  const setRole = (m, role) => memberAction(m.peerId, () => App.SetGroupRole(conv.groupId, m.peerId, role));

  async function kick(m, ban) {
    if (!(await ask({ message: $t(ban ? 'chats.confirm.ban' : 'chats.confirm.remove', { name: senderName(m.peerId) }), confirm: $t('common.remove'), danger: true }))) return;
    memberAction(m.peerId, () => App.RemoveGroupMember(conv.groupId, m.peerId, ban));
  }

  async function addMember() {
    const key = newMemberKey.trim();
    if (!key) return;
    await memberAction('add', () => App.AddGroupMember(conv.groupId, key));
    if (!infoError) newMemberKey = '';
  }

  async function leave() {
    if (!(await ask({ message: $t('chats.confirm.leave', { name: conv.name }), danger: true }))) return;
    try { await leaveGroup(conv); } catch (err) { infoError = errText(err); }
  }

  $: names = Object.fromEntries($chats.filter((c) => c.kind === 'direct').map((c) => [c.peerId, c.name]));
  $: senderName = (id) => names[id] || (id ? short(id, 4) : '');
  const canModerate = (m) => !m.isMe && myRank >= RANK.moderator && myRank > (RANK[m.role] || 0);
  const canAssign = (m) => !m.isMe && canManage && myRank > (RANK[m.role] || 0);
  const assignable = (m) => ASSIGNABLE.filter((r) => RANK[r] < myRank || r === m.role);

  // ---- search inside the chat ------------------------------------------------------------------
  let searching = false;
  let query = '';
  let searchInput;
  let cur = -1; // index into matches (oldest first); a new search starts at the newest
  let lastQ = '';
  $: q = searching ? query.trim().toLowerCase() : '';
  const haystack = (m) => ((m.file ? m.file.name + ' ' : '') + (m.text || '')).toLowerCase();
  $: matches = q ? $messages.filter((m) => !m.system && haystack(m).includes(q)).map((m) => m.id) : [];
  $: if (q !== lastQ) {
    lastQ = q;
    cur = matches.length - 1;
    if (cur >= 0) reveal(matches[cur], false);
  }
  $: currentMatch = cur >= 0 ? matches[cur] : '';

  async function openSearch() {
    searching = true;
    await tick();
    searchInput?.focus();
    searchInput?.select();
  }
  function closeSearch() {
    searching = false;
    query = '';
    cur = -1;
  }
  function step(dir) {
    if (!matches.length) return;
    cur = (cur + dir + matches.length) % matches.length;
    reveal(matches[cur], false);
  }
  function searchKey(e) {
    if (e.key === 'Enter') { e.preventDefault(); step(e.shiftKey ? 1 : -1); }
    else if (e.key === 'Escape') { e.preventDefault(); closeSearch(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); step(-1); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); step(1); }
  }
  function windowKey(e) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'f') { e.preventDefault(); openSearch(); }
  }

  // scrolls a message into view; flash marks it for a moment
  let flashId = '';
  async function reveal(id, flash = true) {
    await tick();
    const el = scroller?.querySelector(`[data-mid="${CSS.escape(id)}"]`);
    if (!el) return;
    el.scrollIntoView({ block: 'center', behavior: 'smooth' });
    if (flash) {
      flashId = id;
      setTimeout(() => { if (flashId === id) flashId = ''; }, 1600);
    }
  }

  // a saved message opened from the saved list: scroll to it once it is loaded
  let pendingJump = '';
  const unJump = jumpTo.subscribe((j) => {
    if (!j || j.chatId !== conv.id) return;
    jumpTo.set(null);
    if ($messages.some((m) => m.id === j.msgId)) reveal(j.msgId);
    else pendingJump = j.msgId;
  });

  // ---- days --------------------------------------------------------------------------------------
  const dayKey = (ts) => new Date(ts || 0).toDateString();
  function dayLabel(ts) {
    const d = new Date(ts || 0);
    const today = new Date();
    if (d.toDateString() === today.toDateString()) return $t('chats.today');
    if (d.toDateString() === new Date(Date.now() - 864e5).toDateString()) return $t('chats.yesterday');
    return d.toLocaleDateString(intlLocale(), { day: 'numeric', month: 'long', ...(d.getFullYear() === today.getFullYear() ? {} : { year: 'numeric' }) });
  }

  // ---- message menu ------------------------------------------------------------------------------
  let menu = null; // { m, anchor, details }
  let menuNote = '';
  function openMenu(e, m) {
    if (m.system) return;
    if (e.type === 'contextmenu' && e.target.closest('a')) return; // a link keeps its own menu
    e.preventDefault();
    menu = { m, anchor: e.currentTarget.closest('.msg, .comment') || e.currentTarget, details: false };
    menuNote = '';
  }
  async function menuCopy() {
    if (await copy(menu.m.file ? menu.m.file.name : menu.m.text)) {
      menuNote = $t('common.copied');
      setTimeout(() => (menu = null), 600);
    }
  }
  async function menuSave() {
    const on = await toggleSaved(conv, menu.m, senderName(menu.m.sender));
    if (on !== null) {
      menuNote = on ? $t('saved.added') : $t('saved.removed');
      setTimeout(() => (menu = null), 700);
    }
  }
  async function menuSaveFile() {
    const m = menu.m;
    menu = null;
    await saveAsFile(m, conv.name);
  }
  const reacted = (m, emoji) => m.reactions.some((r) => r.emoji === emoji && r.mine);
  function toggleReaction(m, emoji) {
    menu = null;
    react(m.id, emoji, !reacted(m, emoji));
  }
  function menuPin() {
    const m = menu.m;
    menu = null;
    pinMessage(m.id, !m.pinned);
  }
  async function menuDelete() {
    const m = menu.m;
    menu = null;
    const msg = m.mine ? $t('chats.confirm.deleteMsg') : $t('chats.confirm.deleteTheirs', { name: senderName(m.sender) });
    if (!(await ask({ message: msg, confirm: $t('chats.deleteForAll'), danger: true }))) return;
    if (editing && editing.id === m.id) cancelCompose();
    deleteMessage(m.id);
  }
  const fullTime = (ts) => new Date(ts || 0).toLocaleString(intlLocale(), { weekday: 'short', day: 'numeric', month: 'long', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
  const stateOf = (m) => (m.failed ? 'notSent' : m.pending ? (m.state || 'waiting') : 'delivered');
  const authorName = (m) => (m.mine ? $t('chats.You') : conv.kind === 'group' ? senderName(m.sender) : conv.name);

  // ---- links -------------------------------------------------------------------------------------
  function openLink(e, href) {
    e.preventDefault();
    e.stopPropagation();
    openURL(href); // the system browser, never this window
  }

  // ---- files -------------------------------------------------------------------------------------
  $: canSendFiles = (conv.kind === 'group' || !!conv.pubKey) && !conv.readOnly;
  $: chatFiles = Object.values($transfers)
    .filter((x) => (conv.kind === 'group' ? x.group === conv.groupId : x.peer === conv.peerId && !x.group))
    .sort((a, b) => b.created - a.created);
  // the area shrinks when a bar appears above it (pins) or the composer grows: stay at the end
  const ro = new ResizeObserver(() => {
    if (atBottom && scroller) scroller.scrollTop = scroller.scrollHeight;
  });
  onMount(() => {
    if (scroller) ro.observe(scroller);
    // files dropped on the chat are offered to the contact (Wails hands over their paths)
    window.runtime?.OnFileDrop?.((_x, _y, paths) => {
      if (canSendFiles && paths?.length) sendPaths(conv, paths);
    }, true);
  });
  onDestroy(() => {
    ro.disconnect();
    clearTimeout(readTimer);
    unJump();
    window.runtime?.OnFileDropOff?.();
  });

  // ---- info --------------------------------------------------------------------------------------
  $: firstTs = $hasOlder ? 0 : $messages.find((m) => !m.system)?.ts || 0;
  $: savedHere = [...$savedIds].filter((id) => id.startsWith(conv.id + '/')).length;
  $: msgCount = $messages.filter((m) => !m.system && !m.deleted).length;
  const sinceDate = (ts) => (ts ? new Date(ts).toLocaleDateString(intlLocale(), { day: '2-digit', month: '2-digit', year: '2-digit' }) : '—');
</script>

<svelte:window on:keydown={windowKey} />

<div class="conv">
  <div class="main">
    <header class:indent>
      {#if back}
        <button class="icon" title={$t('chats.backToChats')} aria-label={$t('chats.backToChats')} on:click={() => dispatch('back')}><Icon name="arrowLeft" size={19} /></button>
      {/if}
      <button class="who" on:click={toggleInfo} title={$t('chats.chatInfo')}>
        <div class="av">
          <Avatar id={conv.peerId || conv.id} name={conv.name} size={40} />
          {#if conv.kind === 'direct'}<span class="presence" class:on={conv.online}></span>{/if}
        </div>
        <div class="title">
          <h3>{conv.name}</h3>
          <p class="small" class:online={conv.kind === 'direct' && conv.online}>
            {#if conv.kind === 'direct'}{conv.online ? $t('common.online') : $t('common.offline')}{:else}{conv.groupKind === 'channel' ? $t('chats.channelLower') : $t('chats.groupLower')} · {$t('chats.members', { n: conv.members })}{/if}
          </p>
        </div>
      </button>
      {#if $isCallActive && $peerInfo.chatId === conv.id && !$stageExpanded}
        <button class="primary small with-icon" on:click={() => stageExpanded.set(true)}><Icon name="phone" size={14} /> {$t('chats.showCall')}</button>
      {/if}
      <div class="tools">
        <button class="icon" title={$t('chats.call')} aria-label={$t('chats.call')} on:click={() => call(false)} disabled={$isCallActive}><Icon name="phone" size={18} /></button>
        <button class="icon" title={$t('chats.videoCall')} aria-label={$t('chats.videoCall')} on:click={() => call(true)} disabled={$isCallActive}><Icon name="video" size={19} /></button>
        <button class="icon" class:on={searching} title={$t('chats.searchChat')} aria-label={$t('chats.searchChat')} on:click={() => (searching ? closeSearch() : openSearch())}><Icon name="search" size={18} /></button>
        <button class="icon" class:on={info} title={$t('chats.info')} aria-label={$t('chats.info')} on:click={toggleInfo}><Icon name="info" size={19} /></button>
      </div>
    </header>

    {#if searching}
      <div class="findbar" transition:slide={{ duration: 180 }}>
        <Icon name="search" size={15} />
        <input bind:this={searchInput} bind:value={query} on:keydown={searchKey} placeholder={$t('chats.searchPlaceholder')} spellcheck="false" aria-label={$t('chats.searchChat')} />
        <span class="count small" class:none={q && !matches.length}>{#if q}{matches.length ? $t('chats.matchOf', { i: cur + 1, n: matches.length }) : $t('chats.noMatches')}{/if}</span>
        <button class="icon tiny" title={$t('chats.prevMatch')} aria-label={$t('chats.prevMatch')} disabled={!matches.length} on:click={() => step(-1)}><Icon name="chevronUp" size={16} /></button>
        <button class="icon tiny" title={$t('chats.nextMatch')} aria-label={$t('chats.nextMatch')} disabled={!matches.length} on:click={() => step(1)}><Icon name="chevronDown" size={16} /></button>
        <button class="icon tiny" title={$t('common.close')} aria-label={$t('common.close')} on:click={closeSearch}><Icon name="close" size={14} stroke={2.2} /></button>
      </div>
    {/if}

    {#if shownPin}
      <button class="pinbar" on:click={nextPin} title={$t('chats.goToPin')} transition:slide={{ duration: 180 }}>
        <span class="pin-ic"><Icon name="pin" size={15} /></span>
        <span class="grow pin-body">
          <span class="pin-head">{$t('chats.pinned')}{#if $pins.length > 1} · {pinIdx + 1}/{$pins.length}{/if}</span>
          <span class="pin-text">{preview(shownPin)}</span>
        </span>
        {#if canPin}
          <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
          <span class="icon tiny unpin" title={$t('chats.unpin')} on:click|stopPropagation={() => pinMessage(shownPin.id, false)}><Icon name="close" size={13} stroke={2.2} /></span>
        {/if}
      </button>
    {/if}

    {#if isGroup && conv.upgraded === false}
      <p class="small upg">{$t('chats.groupNotUpgraded')}</p>
    {/if}

    <div class="messages" bind:this={scroller} on:scroll={onScroll}>
      {#if $hasOlder}<div class="older small faint">{$t('chats.loadingOlder')}</div>{/if}
      {#each $messages as m, i (m.id)}
        {#if i === 0 || dayKey($messages[i - 1].ts) !== dayKey(m.ts)}
          <div class="day"><span>{dayLabel(m.ts, $lang)}</span></div>
        {/if}
        {#if m.system}
          <div class="system small" data-mid={m.id}>{m.text} · {time(m.ts, $lang)}</div>
        {:else}
          {@const first = !sameGroup($messages[i - 1], m) || dayKey($messages[i - 1].ts) !== dayKey(m.ts)}
          {@const last = !sameGroup(m, $messages[i + 1]) || dayKey($messages[i + 1].ts) !== dayKey(m.ts)}
          {@const isSaved = $savedIds.has(conv.id + '/' + m.id)}
          <!-- svelte-ignore a11y-no-static-element-interactions -->
          <div class="msg" data-mid={m.id} class:mine={m.mine} class:pending={m.pending} class:failed={m.failed} class:first class:last class:gone={m.deleted}
            class:hit={q && matches.includes(m.id)} class:current={m.id === currentMatch} class:flash={m.id === flashId}
            on:contextmenu={(e) => openMenu(e, m)} on:dblclick={() => canReply && !m.deleted && startReply(m)}>
            {#if conv.kind === 'group' && !m.mine && first}<div class="sender small">{senderName(m.sender)}</div>{/if}
            <button class="more" title={$t('chats.msgActions')} aria-label={$t('chats.msgActions')} on:click={(e) => openMenu(e, m)}><Icon name="chevronDown" size={15} /></button>
            {#if m.replyTo && !m.deleted}
              {@const qm = quoteOf(m.replyTo)}
              <button class="quote" on:click|stopPropagation={() => goTo(m.replyTo)} disabled={!qm}>
                {#if qm}
                  <b>{authorName(qm)}</b>
                  <span>{preview(qm)}</span>
                {:else}
                  <span class="faint">{$t('chats.quoteUnavailable')}</span>
                {/if}
              </button>
            {/if}
            {#if m.deleted}
              <span class="text gone-text"><Icon name="trash" size={13} /> {$t('chats.deletedMsg')}</span>
            {:else if m.withheld}
              <span class="text gone-text">{$t('chats.withheld')}</span>
            {:else if m.file}
              <FileCard file={m.file} mine={m.mine} />
            {:else}
              <span class="text">{#each mark(linkify(m.text), q) as p}{#if p.href}<a href={p.href} title={linkHost(p.href)} rel="noreferrer noopener" on:click={(e) => openLink(e, p.href)}>{#if p.mark}<mark>{p.text}</mark>{:else}{p.text}{/if}</a>{:else if p.mark}<mark>{p.text}</mark>{:else}{p.text}{/if}{/each}</span>
            {/if}
            <span class="meta">
              {#if m.pinned}<span class="saved-mark" title={$t('chats.pinned')}><Icon name="pin" size={11} stroke={2.4} /></span>{/if}
              {#if isSaved}<span class="saved-mark" title={$t('saved.saved')}><Icon name="bookmark" size={11} stroke={2.4} /></span>{/if}
              {#if m.edited && !m.deleted}<span class="edited" title={fullTime(m.editedAt)}>{$t('chats.edited')}</span>{/if}
              {time(m.ts, $lang)}
              {#if m.mine && conv.kind === 'direct' && !m.deleted}
                {#if m.failed}<span class="st err" title={$t('chats.st.notSent')}>!</span>
                {:else if m.pending}<span class="st" class:relayed={m.state === 'relayed'} title={$t('chats.st.' + stateOf(m))}><Icon name={m.state === 'sent' || m.state === 'relayed' ? 'check' : 'clock'} size={13} stroke={2.4} /></span>
                {:else}<span class="st" title={$t('chats.st.delivered')}><Icon name="checks" size={14} stroke={2.2} /></span>{/if}
              {/if}
            </span>
            {#if !m.deleted && m.reactions.length}
              <div class="reactions">
                {#each m.reactions as r (r.emoji)}
                  <button class="rx" class:on={r.mine} disabled={!canReact} title={r.by.map((p) => (p === myId ? $t('chats.You') : senderName(p))).join(', ')}
                    on:click|stopPropagation={() => react(m.id, r.emoji, !r.mine)}>{r.emoji}<span>{r.count}</span></button>
                {/each}
              </div>
            {/if}
            {#if !m.deleted && m.replies > 0}
              <button class="replies" on:click|stopPropagation={() => showThread(m)}><Icon name="message" size={13} /> {$t('chats.comments', { n: m.replies })}</button>
            {/if}
          </div>
          {#if m.failed}<div class="fail-note small">{$t('chats.note.notSent')}</div>{:else if m.pending && last}<div class="fail-note small pend">{$t('chats.note.' + stateOf(m))}</div>{/if}
        {/if}
      {:else}
        <div class="empty">
          <Avatar id={conv.peerId || conv.id} name={conv.name} size={64} />
          <p class="small muted">{$t('chats.sayHi', { name: conv.name })}</p>
        </div>
      {/each}
    </div>

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

    {#if conv.kind === 'direct' && conv.blocked}
      <div class="reqbar">
        <p class="small"><Icon name="lock" size={14} /> {$t('chats.blockedNote')}</p>
        <div class="row wrap"><button class="small" on:click={unblock} disabled={reqBusy}>{$t('chats.unblock')}</button></div>
      </div>
    {:else if conv.kind === 'direct' && conv.request}
      <div class="reqbar">
        <p class="small"><b>{$t('chats.requestHead')}</b> {$t('chats.requestNote')}</p>
        <div class="row wrap">
          <input class="grow" bind:value={reqName} placeholder={$t('chats.requestName')} maxlength="64" />
          <button class="primary small" on:click={accept} disabled={reqBusy}>{$t('chats.acceptRequest')}</button>
          <button class="small" on:click={dismiss} disabled={reqBusy}>{$t('chats.dismissRequest')}</button>
          <button class="danger small" on:click={block} disabled={reqBusy}>{$t('chats.block')}</button>
        </div>
        {#if infoError}<p class="small error">{infoError}</p>{/if}
      </div>
    {/if}

    {#if conv.readOnly}
      <p class="small muted bar ro"><Icon name="lock" size={14} /> {$t('chats.readOnly')}</p>
    {:else if conv.kind === 'direct' && conv.blocked}
      <!-- no composer: a blocked person gets nothing either way -->
    {:else}
      <form class="composer" on:submit|preventDefault={submit}>
        {#if replyTo || editing}
          <div class="draft-ctx" transition:slide={{ duration: 150 }}>
            <Icon name={editing ? 'edit' : 'reply'} size={16} />
            <button type="button" class="grow ctx-body" on:click={() => goTo((editing || replyTo).id)}>
              <b>{editing ? $t('chats.editing') : $t('chats.replyingTo', { name: authorName(replyTo) })}</b>
              <span>{preview(editing || replyTo)}</span>
            </button>
            <button type="button" class="icon tiny" title={$t('common.cancel')} aria-label={$t('common.cancel')} on:click={cancelCompose}><Icon name="close" size={14} stroke={2.2} /></button>
          </div>
        {/if}
        <div class="field" class:withattach={canSendFiles && !editing}>
          {#if canSendFiles && !editing}
            <button type="button" class="attach" title={$t('files.attach')} aria-label={$t('files.attach')} on:click={() => pickAndSend(conv)}><Icon name="paperclip" size={18} /></button>
          {/if}
          <textarea bind:this={input} rows="1" placeholder={$t('chats.writeMessage')} bind:value={text} on:keydown={keydown} on:input={grow} />
          <button class="send" class:ready={text.trim()} type="submit" title={$t('chats.send')} aria-label={$t('chats.send')} data-silent disabled={sending || !text.trim()}><Icon name="send" size={17} stroke={2} /></button>
        </div>
      </form>
    {/if}
  </div>

  {#if info}
    <aside class="info" transition:fly={{ x: 40, duration: 280, opacity: 0 }}>
      <div class="info-head">
        <span class="eyebrow grow">{conv.kind === 'direct' ? $t('chats.contact') : conv.groupKind === 'channel' ? $t('chats.channel') : $t('chats.group')}</span>
        <button class="icon" aria-label={$t('chats.closeInfo')} on:click={toggleInfo}><Icon name="close" size={17} /></button>
      </div>
      <div class="profile">
        <Avatar id={conv.peerId || conv.id} name={conv.name} size={76} />
        <h2>{conv.name}</h2>
        <p class="small muted">{#if conv.kind === 'direct'}{conv.online ? $t('common.online') : $t('common.offline')}{:else}{$t('chats.members', { n: conv.members })}{#if conv.myRole} · {$t('chats.youAre', { role: $t('chats.role.' + conv.myRole) })}{/if}{/if}</p>
      </div>

      <div class="facts">
        <div><span class="faint small">{$t('chats.factMessages')}</span><b>{msgCount}{$hasOlder ? '+' : ''}</b></div>
        <div><span class="faint small">{$t('chats.factSince')}</span><b>{sinceDate(firstTs)}</b></div>
        <button class="fact" on:click={() => activeId.set(SAVED_ID)} disabled={!savedHere} title={$t('saved.open')}><span class="faint small">{$t('saved.short')}</span><b>{savedHere}</b></button>
      </div>

      <div class="block">
        <div class="eyebrow">{$t('files.title', { n: chatFiles.length })}</div>
        {#if chatFiles.length}
          <div class="files">
            {#each chatFiles as f (f.id)}
              <div class="frow">
                <Icon name={f.dir === 'out' ? 'upload' : 'download'} size={15} />
                <div class="grow fmeta">
                  <span class="fname" title={f.name}>{f.name}</span>
                  <span class="faint small">{bytes(f.size)} · {f.group && f.dir === 'out' && f.members && !f.fetching && f.state === 'active' ? $t('files.delivered', { n: f.delivered || 0, of: f.members }) : f.state === 'active' ? Math.round((f.done / Math.max(1, f.size)) * 100) + '%' : f.state === 'offered' && f.dir === 'out' ? $t(f.group ? 'files.st.shared' : 'files.st.waitingAccept') : $t('files.st.' + f.state)}</span>
                </div>
                {#if f.state === 'done' && f.dir === 'in'}<button class="icon tiny" title={$t('files.open')} aria-label={$t('files.open')} on:click={() => openFile(f)}><Icon name="external" size={14} /></button>{/if}
                {#if f.path && f.state !== 'failed' && (f.dir === 'out' || f.state === 'done')}<button class="icon tiny" title={$t('files.showInFolder')} aria-label={$t('files.showInFolder')} on:click={() => showInFolder(f.id)}><Icon name="folder" size={14} /></button>{/if}
              </div>
            {/each}
          </div>
        {:else}
          <p class="small faint">{$t('files.none')}</p>
        {/if}
      </div>

      {#if conv.kind === 'direct'}
        <div class="block">
          <div class="eyebrow">{$t('chats.key')}</div>
          <div class="copybox">
            <span class="mono grow">{conv.pubKey || $t('chats.keyUnknown')}</span>
            <button class="icon" title={$t('common.copy')} on:click={() => doCopy('key', conv.pubKey)} disabled={!conv.pubKey}><Icon name={copied === 'key' ? 'check' : 'copy'} size={16} /></button>
          </div>
          <p class="small faint">{$t('chats.e2eContact')}</p>
        </div>

        {#if conv.blocked}
          <button class="with-icon wide" on:click={unblock} disabled={reqBusy}><Icon name="lockOpen" size={16} /> {$t('chats.unblock')}</button>
        {:else}
          <button class="danger with-icon wide" on:click={block} disabled={reqBusy}><Icon name="lock" size={16} /> {$t('chats.block')}</button>
          <p class="small faint">{$t('chats.blockHint')}</p>
        {/if}
        {#if !conv.request}
          <button class="danger with-icon wide" on:click={removeContact}><Icon name="trash" size={16} /> {$t('chats.deleteContact')}</button>
        {/if}
      {:else}
        <div class="block">
          <div class="eyebrow">{$t('chats.invite')}</div>
          <div class="copybox">
            <span class="mono grow clip">{invite}</span>
            <button class="icon" title={$t('chats.copyInvite')} on:click={() => doCopy('invite', invite)} disabled={!invite}><Icon name={copied === 'invite' ? 'check' : 'copy'} size={16} /></button>
          </div>
          <p class="small faint">{$t('chats.inviteHint')}</p>
        </div>

        <div class="block">
          <div class="eyebrow">{$t('chats.membersCount', { n: members.length })}</div>
          <div class="members">
            {#each members as m (m.peerId)}
              <div class="member">
                <div class="row">
                  <Avatar id={m.peerId} name={m.isMe ? $t('common.you') : senderName(m.peerId)} size={30} />
                  <span class="grow nm">{m.isMe ? $t('common.you') : senderName(m.peerId)}</span>
                  {#if canAssign(m)}
                    <select value={m.role} disabled={busyMember === m.peerId} on:change={(e) => setRole(m, e.target.value)}>
                      {#each assignable(m) as r}<option value={r}>{$t('chats.role.' + r)}</option>{/each}
                      {#if !ASSIGNABLE.includes(m.role)}<option value={m.role}>{$t('chats.role.' + m.role)}</option>{/if}
                    </select>
                  {:else}
                    <span class="chip">{$t('chats.role.' + m.role)}</span>
                  {/if}
                </div>
                {#if (conv.myRole === 'owner' && !m.isMe) || canModerate(m)}
                  <div class="row m-acts">
                    {#if conv.myRole === 'owner' && !m.isMe}
                      <button class="ghost small" disabled={busyMember === m.peerId} on:click={() => makeOwner(m)}>{$t('chats.makeOwner')}</button>
                    {/if}
                    {#if canModerate(m)}
                      <button class="ghost small" disabled={busyMember === m.peerId} on:click={() => kick(m, false)}>{$t('common.remove')}</button>
                      {#if myRank >= RANK.admin}<button class="ghost small danger" disabled={busyMember === m.peerId} on:click={() => kick(m, true)}>{$t('chats.ban')}</button>{/if}
                    {/if}
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        </div>

        {#if canManage}
          <div class="block">
            <form class="row" on:submit|preventDefault={addMember}>
              <input class="grow" placeholder={$t('chats.addByKey')} bind:value={newMemberKey} spellcheck="false" />
              <button type="submit" disabled={!newMemberKey.trim() || busyMember === 'add'}>{$t('common.add')}</button>
            </form>
            <p class="small faint">{$t('chats.addHint')}</p>
          </div>
        {/if}
        {#if conv.myRole !== 'owner'}
          <button class="danger with-icon wide" on:click={leave}><Icon name="logout" size={16} /> {$t('chats.leaveGroup')}</button>
        {:else}
          <button class="danger with-icon wide" on:click={deleteGroup}><Icon name="trash" size={16} /> {$t('chats.deleteGroup')}</button>
          <p class="small faint">{$t('chats.ownerLeaveHint')}</p>
        {/if}
      {/if}
      {#if infoError}<p class="error small">{infoError}</p>{/if}
    </aside>
  {/if}

  {#if $thread && !info}
    <aside class="info thread" transition:fly={{ x: 40, duration: 280, opacity: 0 }}>
      <div class="info-head">
        <span class="eyebrow grow">{$t('chats.threadTitle')}</span>
        <button class="icon" aria-label={$t('common.close')} on:click={closeThread}><Icon name="close" size={17} /></button>
      </div>
      {#if threadRoot}
        <button class="root" on:click={() => goTo(threadRoot.id)}>
          <b class="small">{authorName(threadRoot)}</b>
          <span>{threadRoot.deleted ? $t('chats.deletedMsg') : preview(threadRoot)}</span>
          <span class="faint small">{time(threadRoot.ts, $lang)}</span>
        </button>
      {/if}
      <div class="comments">
        {#each $thread.messages as c (c.id)}
          <!-- svelte-ignore a11y-no-static-element-interactions -->
          <div class="comment" class:mine={c.mine} data-cid={c.id} on:contextmenu={(e) => openMenu(e, c)}>
            <Avatar id={c.mine ? myId : c.sender} name={authorName(c)} size={28} />
            <div class="grow c-body">
              <div class="c-head small"><b>{authorName(c)}</b><span class="faint">{time(c.ts, $lang)}{#if c.edited && !c.deleted} · {$t('chats.edited')}{/if}</span>
                <button class="more" title={$t('chats.msgActions')} aria-label={$t('chats.msgActions')} on:click={(e) => openMenu(e, c)}><Icon name="chevronDown" size={14} /></button>
              </div>
              {#if c.deleted}
                <span class="gone-text small">{$t('chats.deletedMsg')}</span>
              {:else}
                <span class="text">{#each linkify(c.text) as p}{#if p.href}<a href={p.href} title={linkHost(p.href)} rel="noreferrer noopener" on:click={(e) => openLink(e, p.href)}>{p.text}</a>{:else}{p.text}{/if}{/each}</span>
              {/if}
              {#if !c.deleted && c.reactions.length}
                <div class="reactions">
                  {#each c.reactions as r (r.emoji)}
                    <button class="rx" class:on={r.mine} disabled={!canReact} on:click={() => react(c.id, r.emoji, !r.mine)}>{r.emoji}<span>{r.count}</span></button>
                  {/each}
                </div>
              {/if}
              {#if c.mine && conv.kind === 'direct' && c.pending}<span class="small faint">{$t('chats.note.' + stateOf(c))}</span>{/if}
            </div>
          </div>
        {:else}
          <p class="small faint">{$t('chats.noComments')}</p>
        {/each}
      </div>
      {#if canReply}
        <form class="composer thread-composer" on:submit|preventDefault={sendComment}>
          {#if editingComment}
            <div class="draft-ctx">
              <Icon name="edit" size={16} />
              <span class="grow ctx-body"><b>{$t('chats.editing')}</b><span>{preview(editingComment)}</span></span>
              <button type="button" class="icon tiny" title={$t('common.cancel')} aria-label={$t('common.cancel')} on:click={() => { editingComment = null; threadText = ''; }}><Icon name="close" size={14} stroke={2.2} /></button>
            </div>
          {/if}
          <div class="field">
            <textarea bind:this={threadInput} rows="1" placeholder={$t('chats.writeComment')} bind:value={threadText} on:keydown={threadKey} />
            <button class="send" class:ready={threadText.trim()} type="submit" title={$t('chats.send')} aria-label={$t('chats.send')} data-silent disabled={threadSending || !threadText.trim()}><Icon name="send" size={17} stroke={2} /></button>
          </div>
        </form>
      {/if}
    </aside>
  {/if}

  {#if menu}
    <Popover anchor={menu.anchor} width={270} label={$t('chats.msgActions')} on:close={() => (menu = null)}>
      <div class="menu">
        {#if menu.details}
          <div class="details">
            <div><span class="faint small">{$t('chats.det.sent')}</span><span>{fullTime(menu.m.ts)}</span></div>
            <div><span class="faint small">{$t('chats.det.from')}</span><span>{authorName(menu.m)}</span></div>
            {#if menu.m.edited}<div><span class="faint small">{$t('chats.det.edited')}</span><span>{fullTime(menu.m.editedAt)}</span></div>{/if}
            {#if menu.m.mine && conv.kind === 'direct'}<div><span class="faint small">{$t('chats.det.state')}</span><span>{$t('chats.st.' + stateOf(menu.m))}</span></div>{/if}
            {#if menu.m.file}<div><span class="faint small">{$t('chats.det.file')}</span><span>{menu.m.file.name} · {bytes(menu.m.file.size)}</span></div>{/if}
            <div><span class="faint small">{$t('chats.det.id')}</span><span class="mono tiny-id">{menu.m.id}</span></div>
            <p class="small faint">{$t('chats.det.e2e')}</p>
          </div>
        {:else}
          {@const m = menu.m}
          {#if canReact && !m.deleted}
            <div class="quick">
              {#each QUICK as emoji}
                <button class="qr" class:on={reacted(m, emoji)} title={emoji} on:click={() => toggleReaction(m, emoji)}>{emoji}</button>
              {/each}
            </div>
          {/if}
          {#if canReply && !m.deleted && !m.thread}<button class="item" on:click={() => startReply(m)}><Icon name="reply" size={16} /> {$t('chats.reply')}</button>{/if}
          {#if canReply && !m.deleted && !m.thread}<button class="item" on:click={() => showThread(m)}><Icon name="message" size={16} /> {m.replies ? $t('chats.comments', { n: m.replies }) : $t('chats.comment')}</button>{/if}
          {#if canEdit(m)}<button class="item" on:click={() => (m.thread ? editComment(m) : startEdit(m))}><Icon name="edit" size={16} /> {$t('chats.edit')}</button>{/if}
          {#if canPin && !m.deleted && !m.thread}<button class="item" on:click={menuPin}><Icon name="pin" size={16} /> {m.pinned ? $t('chats.unpin') : $t('chats.pin')}</button>{/if}
          {#if !m.deleted}
            <button class="item" on:click={menuCopy}><Icon name="copy" size={16} /> {m.file ? $t('chats.copyName') : $t('chats.copyText')}</button>
            {#if !m.thread}<button class="item" on:click={menuSave}><Icon name="bookmark" size={16} /> {$savedIds.has(conv.id + '/' + m.id) ? $t('saved.remove') : $t('saved.add')}</button>{/if}
            {#if !m.file && m.text}<button class="item" on:click={menuSaveFile}><Icon name="download" size={16} /> {$t('chats.saveAsFile')}</button>{/if}
          {/if}
          <button class="item" on:click={() => (menu = { ...menu, details: true })}><Icon name="info" size={16} /> {$t('chats.details')}</button>
          {#if canDelete(m)}<button class="item danger-item" on:click={menuDelete}><Icon name="trash" size={16} /> {$t('chats.deleteForAll')}</button>{/if}
          {#if conv.kind === 'direct' && conv.upgraded === false && !m.deleted}<p class="small faint menu-hint">{$t('chats.oldPeerHint')}</p>{/if}
          {#if menuNote}<p class="small note-ok">{menuNote}</p>{/if}
        {/if}
      </div>
    </Popover>
  {/if}
</div>

<style>
  .conv { flex: 1; min-height: 0; display: flex; position: relative; }
  .main { flex: 1; min-width: 0; display: flex; flex-direction: column; --wails-drop-target: drop; }

  header {
    display: flex; align-items: center; gap: 8px; padding: 10px 14px 10px 14px; min-height: 64px;
    border-bottom: 1px solid var(--line-soft); background: var(--glass); backdrop-filter: blur(16px) saturate(1.3);
    position: relative; z-index: 2;
  }
  header.indent { padding-left: 58px; }
  .who {
    flex: 1; min-width: 0; display: flex; align-items: center; gap: 12px; padding: 4px 8px 4px 4px;
    background: transparent; border: none; text-align: left; font-weight: inherit; border-radius: 14px;
  }
  .who:hover:not(:disabled) { background: var(--hover); border: none; }
  .who:active:not(:disabled) { transform: scale(0.99); }
  .av { position: relative; flex: none; }
  .presence { position: absolute; right: -1px; bottom: -1px; width: 12px; height: 12px; border-radius: 50%; background: var(--faint); border: 2.5px solid var(--panel); }
  .presence.on { background: var(--ok); }
  .title { min-width: 0; }
  .title h3 { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .title p { color: var(--faint); }
  .title p.online { color: var(--ok); }
  .tools { display: flex; gap: 2px; }
  .tools .icon { width: 38px; height: 38px; }

  .findbar {
    display: flex; align-items: center; gap: 4px; padding: 5px 10px 5px 16px; color: var(--faint);
    border-bottom: 1px solid var(--line-soft); background: var(--panel); position: relative; z-index: 1;
  }
  .findbar input { flex: 1; min-width: 0; border: none; background: transparent; box-shadow: none; padding: 6px 6px; }
  .findbar input:focus { box-shadow: none; background: transparent; }
  .findbar .count { color: var(--muted); font-variant-numeric: tabular-nums; white-space: nowrap; padding: 0 6px; }
  .findbar .count.none { color: var(--danger); }
  .icon.tiny { width: 28px; height: 28px; border-radius: 8px; }

  .messages { flex: 1; overflow-y: auto; overflow-x: hidden; padding: 18px clamp(12px, 3vw, 28px) 10px; display: flex; flex-direction: column; }
  :global(.wails-drop-target-active) .messages { outline: 2px dashed var(--accent); outline-offset: -8px; border-radius: 16px; }
  .day { align-self: center; margin: 14px 0 4px; }
  .day span {
    display: inline-block; padding: 3px 11px; border-radius: 999px; font-size: 11.5px; font-weight: 600; color: var(--muted);
    background: var(--glass); backdrop-filter: blur(10px); border: 1px solid var(--line-soft);
  }
  .msg {
    position: relative; max-width: min(76%, 640px); align-self: flex-start;
    background: var(--bubble-in); border: 1px solid var(--line-soft); box-shadow: var(--shadow-sm);
    border-radius: 6px 18px 18px 6px; padding: 7px 12px 7px; margin-top: 2px;
    animation: msg-in 0.32s var(--spring) both; transform-origin: bottom left; transition: box-shadow 0.2s;
  }
  .msg.first { border-top-left-radius: 18px; margin-top: 10px; }
  .msg.last { border-bottom-left-radius: 18px; }
  .msg.first.last { border-bottom-left-radius: 6px; }
  .msg.mine {
    align-self: flex-end; border: none; transform-origin: bottom right;
    background: linear-gradient(135deg, var(--accent), var(--accent-2));
    color: #fff; border-radius: 18px 6px 6px 18px;
  }
  .msg.mine.first { border-top-right-radius: 18px; }
  .msg.mine.last { border-bottom-right-radius: 18px; }
  .msg.mine.first.last { border-bottom-right-radius: 6px; }
  .msg.pending { opacity: 0.72; }
  .msg.failed { background: var(--danger); }
  .msg.hit { box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent) 35%, transparent); }
  .msg.current { box-shadow: 0 0 0 2px var(--accent), var(--glow); }
  .msg.flash { animation: flash 1.6s ease-out; }
  @keyframes flash { 0%, 45% { box-shadow: 0 0 0 3px var(--accent), var(--glow); } 100% { box-shadow: var(--shadow-sm); } }
  @keyframes msg-in { from { opacity: 0; transform: translateY(8px) scale(0.96); } to { opacity: 1; transform: none; } }
  .msg .more {
    position: absolute; top: 4px; right: 4px; width: 22px; height: 22px; padding: 0; border-radius: 7px; border: none;
    display: grid; place-items: center; opacity: 0; background: var(--panel); color: var(--muted); box-shadow: var(--shadow-sm);
    transition: opacity 0.15s; z-index: 1;
  }
  .msg:hover .more, .msg .more:focus-visible { opacity: 1; }
  .msg.mine .more { background: rgba(0, 0, 0, 0.25); color: #fff; box-shadow: none; }
  .text { white-space: pre-wrap; word-wrap: break-word; overflow-wrap: anywhere; line-height: 1.45; }
  .text mark { background: color-mix(in srgb, #ffd54a 70%, transparent); color: inherit; border-radius: 3px; padding: 0 1px; }
  .msg.current .text mark { background: #ffb300; color: #111; }
  .text a { color: var(--accent); text-decoration: underline; text-underline-offset: 2px; cursor: pointer; }
  .text a:hover { text-decoration-thickness: 2px; }
  .msg.mine .text a { color: #fff; }
  .sender { color: var(--accent); font-weight: 650; margin-bottom: 1px; }
  .meta {
    float: right; display: inline-flex; align-items: center; gap: 3px; margin: 6px 0 -3px 10px;
    color: var(--faint); font-size: 10.5px; font-variant-numeric: tabular-nums; line-height: 1;
  }
  .msg.mine .meta { color: rgba(255, 255, 255, 0.72); }
  .saved-mark { display: inline-flex; color: var(--accent); }
  .msg.mine .saved-mark { color: #fff; }
  .st { display: inline-flex; }
  .st.err { font-weight: 800; color: #fff; }
  .fail-note { align-self: flex-end; color: var(--danger); font-size: 11px; margin: 2px 4px 0; }
  .fail-note.pend { color: var(--faint); }
  .system { align-self: center; padding: 4px 12px; border-radius: 999px; background: var(--raised); color: var(--muted); margin: 14px 0 6px; font-size: 12px; }
  .empty { margin: auto; display: flex; flex-direction: column; align-items: center; gap: 12px; animation: fade-up 0.4s var(--ease); }

  .bar { padding: 6px 20px; }
  .callbar {
    display: flex; align-items: center; gap: 10px;
    margin: 0 16px 8px; padding: 8px 12px;
    border: 1px solid color-mix(in srgb, var(--ok) 40%, transparent);
    background: var(--ok-soft);
    border-radius: 12px;
  }
  .callbar .small { flex: 1; }
  .pulse { width: 9px; height: 9px; border-radius: 50%; background: var(--ok); animation: pulse 1.6s ease-in-out infinite; }
  @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.35; } }
  .ro { display: flex; align-items: center; gap: 8px; justify-content: center; padding: 14px; border-top: 1px solid var(--line-soft); }
  .composer { padding: 8px clamp(10px, 2.5vw, 20px) 14px; }
  .field {
    display: flex; align-items: flex-end; gap: 6px; padding: 5px 5px 5px 16px;
    background: var(--panel); border: 1px solid var(--line); border-radius: 22px; box-shadow: var(--shadow-sm);
    transition: border-color var(--dur), box-shadow var(--dur);
  }
  .field.withattach { padding-left: 5px; }
  .field:focus-within { border-color: var(--accent); box-shadow: 0 0 0 4px var(--accent-soft); }
  .field textarea { flex: 1; resize: none; max-height: 160px; border: none; background: transparent; padding: 8px 0; box-shadow: none; min-height: 38px; }
  .field textarea:focus { box-shadow: none; background: transparent; }
  .attach { width: 38px; height: 38px; padding: 0; border-radius: 50%; border: none; flex: none; display: grid; place-items: center; background: transparent; color: var(--muted); }
  .attach:hover:not(:disabled) { color: var(--accent); background: var(--accent-soft); border: none; }
  .send {
    width: 38px; height: 38px; padding: 0; border-radius: 50%; border: none; flex: none;
    display: grid; place-items: center; color: var(--faint); background: var(--raised);
    transform: scale(0.9);
  }
  .send.ready { color: #fff; background: linear-gradient(135deg, var(--accent), var(--accent-2)); box-shadow: var(--glow); transform: scale(1); }
  .send:disabled { opacity: 1; }
  .send.ready:hover:not(:disabled) { filter: brightness(1.1); transform: scale(1.06) rotate(-8deg); background: linear-gradient(135deg, var(--accent), var(--accent-2)); }

  .info {
    width: 320px; flex: none; overflow-y: auto; display: flex; flex-direction: column; gap: 16px;
    padding: 12px 16px 20px; border-left: 1px solid var(--line-soft); background: var(--panel);
  }
  @media (max-width: 1100px) {
    .info { position: absolute; right: 0; top: 0; bottom: 0; z-index: 10; box-shadow: var(--shadow-lg); max-width: 100%; }
  }
  .info-head { display: flex; align-items: center; min-height: 40px; }
  .profile { display: flex; flex-direction: column; align-items: center; gap: 6px; text-align: center; padding: 4px 0 6px; }
  .profile h2 { margin-top: 6px; font-size: 18px; }
  .block { display: flex; flex-direction: column; gap: 8px; }
  .copybox { display: flex; align-items: center; gap: 6px; padding: 8px 6px 8px 12px; border-radius: 12px; background: var(--raised); }
  .clip { max-height: 3.4em; overflow: hidden; }
  .members { display: flex; flex-direction: column; gap: 2px; }
  .member { padding: 6px 6px; border-radius: 12px; transition: background var(--dur); }
  .member:hover { background: var(--hover); }
  .member .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 550; }
  .member select { width: auto; padding: 4px 28px 4px 10px; font-size: 12px; border-radius: 9px; background-position: right 8px center; }
  .m-acts { padding: 4px 0 0 38px; gap: 4px; flex-wrap: wrap; }
  .wide { justify-content: center; width: 100%; }

  .facts { display: grid; grid-template-columns: repeat(3, 1fr); gap: 6px; }
  .facts > div, .fact {
    display: flex; flex-direction: column; align-items: flex-start; gap: 2px; padding: 8px 10px; min-width: 0;
    border-radius: 12px; background: var(--raised); text-align: left; font-weight: 400; border: none;
  }
  .facts b { font-size: 13px; font-weight: 650; color: var(--text); max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .fact:hover:not(:disabled) { background: var(--accent-soft); border: none; }
  .fact:disabled { opacity: 1; }
  .files { display: flex; flex-direction: column; gap: 2px; }
  .frow { display: flex; align-items: center; gap: 8px; padding: 6px 4px 6px 8px; border-radius: 10px; color: var(--muted); }
  .frow:hover { background: var(--hover); }
  .fmeta { min-width: 0; display: flex; flex-direction: column; }
  .fname { color: var(--text); font-weight: 550; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }

  .menu { display: flex; flex-direction: column; padding: 6px; gap: 2px; }
  .item { display: flex; align-items: center; gap: 10px; width: 100%; padding: 8px 10px; border: none; border-radius: 10px; background: transparent; text-align: left; font-weight: 500; }
  .item:hover:not(:disabled) { background: var(--hover); border: none; }
  .note-ok { color: var(--ok); padding: 2px 10px 4px; }
  .details { display: flex; flex-direction: column; gap: 9px; padding: 6px 8px 8px; }
  .details > div { display: flex; flex-direction: column; gap: 1px; }
  .tiny-id { font-size: 11px; word-break: break-all; }
  /* pins, the old-group banner, older pages */
  .pinbar {
    display: flex; align-items: center; gap: 10px; width: 100%; padding: 6px 12px 6px 16px; border: none; border-radius: 0;
    border-bottom: 1px solid var(--line-soft); background: var(--panel); text-align: left; font-weight: 400; position: relative; z-index: 1;
  }
  .pinbar:hover:not(:disabled) { background: var(--hover); border: none; border-bottom: 1px solid var(--line-soft); }
  .pin-ic { color: var(--accent); display: inline-flex; }
  .pin-body { min-width: 0; display: flex; flex-direction: column; border-left: 2px solid var(--accent); padding-left: 8px; }
  .pin-head { color: var(--accent); font-size: 12px; font-weight: 650; }
  .pin-text { font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .unpin { width: 26px; height: 26px; display: grid; place-items: center; color: var(--faint); border-radius: 8px; }
  .unpin:hover { background: var(--raised); color: var(--text); }
  .upg { margin: 0; padding: 7px 18px; background: var(--warn-soft, var(--raised)); color: var(--muted); border-bottom: 1px solid var(--line-soft); }
  .older { align-self: center; padding: 6px 0 2px; }

  /* answers, deleted and edited messages, reactions, comment counts */
  .quote {
    display: flex; flex-direction: column; align-items: flex-start; gap: 0; width: 100%; min-width: 120px; margin: 2px 0 5px; padding: 3px 8px;
    border: none; border-left: 3px solid var(--accent); border-radius: 6px; background: var(--accent-soft); text-align: left; font-weight: 400;
    font-size: 12.5px; line-height: 1.35; color: inherit;
  }
  .quote:hover:not(:disabled) { border: none; border-left: 3px solid var(--accent); filter: brightness(1.05); background: var(--accent-soft); }
  .quote:disabled { opacity: 1; cursor: default; }
  .quote b { color: var(--accent); font-weight: 650; }
  .quote span { max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .msg.mine .quote { background: rgba(255, 255, 255, 0.16); border-left-color: rgba(255, 255, 255, 0.85); }
  .msg.mine .quote b { color: #fff; }
  .gone-text { display: inline-flex; align-items: center; gap: 5px; font-style: italic; color: var(--faint); }
  .msg.gone { opacity: 0.85; }
  .msg.mine .gone-text { color: rgba(255, 255, 255, 0.8); }
  .edited { font-style: italic; margin-right: 2px; }
  .st.relayed { opacity: 0.65; }
  .reactions { clear: both; display: flex; flex-wrap: wrap; gap: 4px; padding-top: 6px; }
  .rx {
    display: inline-flex; align-items: center; gap: 4px; padding: 2px 8px; border-radius: 999px; font-size: 13px; font-weight: 500; line-height: 1.5;
    border: 1px solid var(--line-soft); background: var(--raised); color: var(--text);
  }
  .rx span { font-size: 11.5px; font-variant-numeric: tabular-nums; color: var(--muted); }
  .rx.on { border-color: var(--accent); background: var(--accent-soft); }
  .rx.on span { color: var(--accent); font-weight: 650; }
  .rx:disabled { opacity: 1; cursor: default; }
  .msg.mine .rx { background: rgba(255, 255, 255, 0.16); border-color: transparent; color: #fff; }
  .msg.mine .rx span { color: rgba(255, 255, 255, 0.85); }
  .msg.mine .rx.on { background: rgba(255, 255, 255, 0.32); border-color: rgba(255, 255, 255, 0.7); }
  .replies {
    clear: both; display: flex; align-items: center; gap: 6px; margin-top: 6px; padding: 4px 2px 0; width: 100%;
    border: none; border-top: 1px solid var(--line-soft); border-radius: 0; background: transparent; color: var(--accent); font-size: 12.5px; font-weight: 600;
  }
  .replies:hover:not(:disabled) { background: transparent; border: none; border-top: 1px solid var(--line-soft); text-decoration: underline; }
  .msg.mine .replies { color: #fff; border-top-color: rgba(255, 255, 255, 0.25); }

  /* the answer or edit under way, above the composer */
  .draft-ctx { display: flex; align-items: center; gap: 10px; padding: 4px 6px 8px 14px; color: var(--accent); }
  .ctx-body {
    min-width: 0; display: flex; flex-direction: column; align-items: flex-start; padding: 0 0 0 9px; border: none; border-left: 2px solid var(--accent);
    border-radius: 0; background: transparent; text-align: left; font-weight: 400; color: var(--text);
  }
  .ctx-body:hover:not(:disabled) { background: transparent; border: none; border-left: 2px solid var(--accent); }
  .ctx-body b { color: var(--accent); font-size: 12.5px; font-weight: 650; }
  .ctx-body span { max-width: 100%; font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  /* comments */
  .thread { gap: 10px; padding-bottom: 0; }
  .root {
    display: flex; flex-direction: column; align-items: flex-start; gap: 2px; padding: 10px 12px; border: none; border-left: 3px solid var(--accent);
    border-radius: 10px; background: var(--raised); text-align: left; font-weight: 400;
  }
  .root:hover:not(:disabled) { border: none; border-left: 3px solid var(--accent); background: var(--hover); }
  .root span:not(.faint) { white-space: pre-wrap; overflow-wrap: anywhere; max-height: 7.5em; overflow: hidden; }
  .comments { flex: 1; display: flex; flex-direction: column; gap: 2px; }
  .comment { display: flex; gap: 10px; padding: 7px 6px; border-radius: 10px; position: relative; }
  .comment:hover { background: var(--hover); }
  .c-body { min-width: 0; display: flex; flex-direction: column; gap: 2px; }
  .c-head { display: flex; align-items: baseline; gap: 6px; }
  .c-head b { font-weight: 650; }
  .comment .more { position: absolute; top: 6px; right: 6px; opacity: 0; width: 22px; height: 22px; padding: 0; border-radius: 7px; border: none; display: grid; place-items: center; background: var(--panel); color: var(--muted); }
  .comment:hover .more, .comment .more:focus-visible { opacity: 1; }
  .thread-composer { position: sticky; bottom: 0; padding: 8px 0 14px; background: var(--panel); }
  .thread-composer .field { padding-left: 14px; }

  /* the message menu */
  .quick { display: flex; justify-content: space-between; gap: 2px; padding: 2px 4px 6px; margin-bottom: 2px; border-bottom: 1px solid var(--line-soft); }
  .qr { width: 36px; height: 36px; padding: 0; border: none; border-radius: 50%; background: transparent; font-size: 19px; line-height: 1; display: grid; place-items: center; }
  .qr:hover:not(:disabled) { background: var(--hover); border: none; transform: scale(1.12); }
  .qr.on { background: var(--accent-soft); }
  .danger-item { color: var(--danger); }
  .menu-hint { padding: 4px 10px 2px; line-height: 1.4; }
  .reqbar { display: flex; flex-direction: column; gap: 8px; margin: 0 16px 8px; padding: 10px 12px; border-radius: 12px; background: var(--raised); }
  .reqbar p { margin: 0; line-height: 1.45; }
  .reqbar input { min-width: 140px; }
</style>
