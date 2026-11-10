<script>
  import { createEventDispatcher, tick } from 'svelte';
  import { chats, activeId, openChat, channels, activeChannel, mainChannel, openChannel, createChannel, createCategory, removeChannel, saveChannel, saveMainChannel, moveChannel, placeChannel, setupCategories } from '../../stores/chats.js';
  import { isCallActive, peerInfo, stageExpanded } from '../../stores/call.store.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Icon from '../../components/ui/Icon.svelte';
  import Popover from '../../components/ui/Popover.svelte';
  import ChannelRow from './ChannelRow.svelte';
  import ChannelSettings from './ChannelSettings.svelte';
  import IconButton from '../../components/ui/IconButton.svelte';
  import Menu from '../../components/ui/Menu.svelte';
  import { panelHidden, togglePanel } from '../../stores/panels.js';
  import SelfCard from './SelfCard.svelte';
  import Avatar from '../../components/ui/Avatar.svelte';
  import { App } from '../../api.js';

  /** A space's column: its name and settings, then its categories, each with text channels and
   *  voice rooms (the main channel among them). A space without categories shows two headings,
   *  text and voice; the first change to either (a rename, a removal, a channel added to it) makes
   *  them real categories (ChatCategoriesSetup). Channels in no category sit on top. Who may manage
   *  channels (conv.can.channels) adds, sets up, reorders, files and removes them. */
  export let conv;
  const hid = panelHidden.space;

  const dispatch = createEventDispatcher();
  const MAIN = { id: '', main: true };
  const kind = (c) => (c.main ? 3 : c.category ? 2 : c.voice ? 1 : 0);

  $: cats = $channels.filter((c) => c.category);
  $: catIds = new Set(cats.map((c) => c.id));
  $: parentOf = (c) => {
    const p = c.main ? $mainChannel.parent : c.parent;
    return p && catIds.has(p) ? p : '';
  };
  $: itemsIn = (id) => [
    ...(parentOf(MAIN) === id ? [MAIN] : []),
    ...$channels.filter((c) => !c.voice && !c.category && parentOf(c) === id),
    ...$channels.filter((c) => c.voice && parentOf(c) === id),
  ];
  // what the column shows: virtual headings in a space without categories, else the channels in no
  // category on top and the categories under them
  $: sections = cats.length
    ? [{ key: 'loose', loose: true, items: itemsIn('') }, ...cats.map((c) => ({ key: c.id, cat: c, name: c.name, items: itemsIn(c.id) }))]
    : [
        { key: 'v:text', virtual: 'text', name: $t('layout.headText'), items: [MAIN, ...$channels.filter((c) => !c.voice && !c.category)] },
        { key: 'v:voice', virtual: 'voice', name: $t('layout.headVoice'), items: $channels.filter((c) => c.voice) },
      ];
  $: textChannels = $channels.filter((c) => !c.voice && !c.category);
  function openMain() {
    if ($isCallActive && $peerInfo.chatId === conv.id) stageExpanded.set(false);
    openChannel('');
  }
  $: inRoom = $isCallActive && $peerInfo.chatId === conv.id ? $peerInfo.room || '' : '';
  $: manage = !!(conv.can.channels ?? conv.can.manage);
  $: mainName = $mainChannel.name || $t('chats.mainChannel');

  // ---- the column's own filter (all / text / voice) and search ------------------------------------
  let filter = 'all';
  let query = '';
  $: q = query.trim().toLowerCase();
  $: shownItems = (sec) => sec.items.filter((ch) => {
    if (filter === 'text' && ch.voice) return false;
    if (filter === 'voice' && !ch.voice) return false;
    return !q || (ch.main ? mainName : ch.name).toLowerCase().includes(q);
  });
  $: showSec = (sec) => (filter === 'all' && !q) || shownItems(sec).length > 0 || adding?.sec?.key === sec.key;
  $: anyShown = sections.some((sec) => shownItems(sec).length > 0);
  $: sortedChats = [...$chats].sort((a, b) => (b.ts || 0) - (a.ts || 0));

  // ---- switching to another chat without leaving the column ---------------------------------------
  let switchBtn;
  let switching = false;
  function pick(id) {
    switching = false;
    openChat(id);
  }
  function allChats() {
    switching = false;
    activeId.set('');
  }

  // the real category behind a section: a virtual heading becomes one first
  async function categoryOf(sec) {
    if (sec.loose) return '';
    if (!sec.virtual) return sec.cat.id;
    const ids = await setupCategories();
    return ids ? ids[sec.virtual === 'text' ? 0 : 1] : null;
  }

  // ---- folded sections, remembered per space on this device --------------------------------------
  const foldKey = () => 'ef.chanfold.' + conv.id;
  let folded = new Set();
  $: conv.id, loadFolded();
  function loadFolded() {
    try { folded = new Set(JSON.parse(localStorage.getItem(foldKey()) || '[]')); } catch (_) { folded = new Set(); }
  }
  function fold(key) {
    folded.has(key) ? folded.delete(key) : folded.add(key);
    folded = folded;
    try { localStorage.setItem(foldKey(), JSON.stringify([...folded])); } catch (_) { /* not kept */ }
  }

  // ---- a new channel, room or category: its name typed in place ----------------------------------
  let adding = null; // { kind: 'text' | 'voice' | 'category', sec }
  let name = '';
  let input;
  async function startAdd(k, sec = null) {
    adding = { kind: k, sec };
    name = '';
    if (sec && folded.has(sec.key)) fold(sec.key);
    await tick();
    input?.focus();
  }
  // Enter and leaving the field both end the typing: whichever comes first makes it
  async function add() {
    const a = adding;
    const n = name.trim();
    adding = null;
    name = '';
    if (!a || !n) return;
    if (a.kind === 'category') return createCategory(n);
    const parent = await categoryOf(a.sec);
    if (parent === null) return;
    const ch = await createChannel(n, a.kind === 'voice', parent);
    if (ch && !ch.voice) openChannel(ch.id);
  }
  function key(e) {
    if (e.key === 'Escape') { e.preventDefault(); adding = null; }
  }

  // ---- settings of a channel, a category (a heading) or the main channel -------------------------
  let editing = null; // { ch, sec, main, name, topic, readOnly, slowSecs, parent, open, overrides }
  let roles = []; // the group's roles but the owner's, for a channel's permissions
  async function loadRoles() {
    try { roles = ((await App.GroupRoles(conv.groupId)) || []).filter((r) => r.rank < 100); } catch (_) { roles = []; }
  }
  function edit(e, ch, sec = null) {
    e.preventDefault();
    e.stopPropagation();
    if (!manage) return;
    if (sec) return (editing = { sec, ch: sec.cat || { category: true }, name: sec.name });
    const main = !!ch.main;
    const src = main ? $mainChannel : ch;
    editing = { ch, main, name: main ? src.name || '' : src.name, topic: src.topic || '', readOnly: !!src.readOnly,
      slowSecs: src.slowSecs || 0, parent: parentOf(ch), open: main ? src.open || '' : '', overrides: [...(src.overrides || [])] };
    loadRoles();
  }
  async function saveEdit() {
    const ed = editing;
    if (!ed.main && !ed.name.trim()) return;
    editing = null;
    if (ed.sec) {
      const id = await categoryOf(ed.sec);
      if (!id) return;
      const cat = ed.sec.cat || { id, category: true, position: ed.sec.virtual === 'voice' ? 1 : 0 };
      return saveChannel({ ...cat, id, name: ed.name });
    }
    if (ed.main) {
      await saveMainChannel(ed);
      if (ed.parent !== parentOf(MAIN)) await placeChannel('', ed.parent);
      return;
    }
    await saveChannel({ ...ed.ch, name: ed.name, topic: ed.topic, readOnly: ed.readOnly, slowSecs: ed.slowSecs, parent: ed.parent, overrides: ed.overrides });
  }
  // channels are ordered among their kind; up and down move past the neighbour shown above or below
  function move(dir) {
    const ch = editing.ch;
    editing = null;
    const same = $channels.filter((c) => kind(c) === kind(ch));
    const shown = ch.category ? same : same.filter((c) => parentOf(c) === parentOf(ch));
    const next = shown[shown.findIndex((c) => c.id === ch.id) + dir];
    if (!next) return;
    const rest = same.filter((c) => c.id !== ch.id);
    moveChannel(ch.id, rest.findIndex((c) => c.id === next.id) + (dir > 0 ? 1 : 0));
  }
  async function remove() {
    const ed = editing;
    editing = null;
    const nm = ed.sec ? ed.sec.name : ed.ch.name;
    const msg = ed.ch.category ? 'chats.confirm.removeCategory' : 'chats.confirm.removeChannel';
    if (!(await ask({ message: $t(msg, { name: nm }), confirm: $t('common.remove'), danger: true }))) return;
    const id = ed.sec ? await categoryOf(ed.sec) : ed.ch.id;
    if (id) removeChannel(id);
  }

  // ---- dragging: reorder among the same kind, or file into a category ----------------------------
  let dragged = null;
  let over = '';
  function dragStart(e, ch) {
    if (!manage) return;
    dragged = ch;
    e.dataTransfer.effectAllowed = 'move';
  }
  function dragOver(e, ch) {
    if (!dragged || dragged.main || kind(dragged) !== kind(ch)) return;
    e.preventDefault();
    over = ch.id;
  }
  function dragOverSec(e, sec) {
    if (!dragged) return;
    if (dragged.category) return sec.cat ? dragOver(e, sec.cat) : undefined;
    e.preventDefault();
    over = 'sec:' + sec.key;
  }
  async function drop(e, ch) {
    e.preventDefault();
    const from = dragged;
    dragged = null;
    over = '';
    if (!from || from.id === ch.id || kind(from) !== kind(ch)) return;
    if (!from.category && parentOf(from) !== parentOf(ch)) await placeChannel(from.id, parentOf(ch));
    const rest = $channels.filter((c) => kind(c) === kind(from) && c.id !== from.id);
    moveChannel(from.id, rest.findIndex((c) => c.id === ch.id));
  }
  async function dropOnSec(e, sec) {
    if (!dragged) return;
    if (dragged.category) return sec.cat ? drop(e, sec.cat) : undefined;
    e.preventDefault();
    const from = dragged;
    dragged = null;
    over = '';
    const parent = await categoryOf(sec);
    if (parent !== null && parentOf(from) !== parent) placeChannel(from.id, parent);
  }
</script>

<nav class="chanbar" aria-label={$t('chats.channels')}>
  <div class="space-head">
    <button class="space-switch" bind:this={switchBtn} class:open={switching} aria-haspopup="menu" aria-expanded={switching} title={$t('layout.switch')} on:click={() => (switching = !switching)}>
      <span class="space-name" title={conv.name}>{conv.name}</span><Icon name="chevronDown" size={15} />
    </button>
    <span class="grow"></span>
    <IconButton icon="panel" size="sm" label={$t($hid ? 'panel.pin' : 'panel.hide') + ' · Ctrl+B'} pressed={$hid} on:click={() => togglePanel('space')} />
    <span class="space-sub">{$t('layout.membersLine', { n: conv.members })}</span>
  </div>
  <div class="chips" role="group" aria-label={$t('chats.channels')}>
    <button class:on={filter === 'all'} aria-pressed={filter === 'all'} on:click={() => (filter = 'all')}>{$t('layout.filter.all')}</button>
    <button class:on={filter === 'text'} aria-pressed={filter === 'text'} on:click={() => (filter = 'text')}>{$t('chats.text')}</button>
    <button class:on={filter === 'voice'} aria-pressed={filter === 'voice'} on:click={() => (filter = 'voice')}>{$t('chats.voice')}</button>
  </div>
  <div class="side-search">
    <Icon name="search" size={15} />
    <input placeholder={$t('layout.searchChannels')} bind:value={query} spellcheck="false" />
    {#if query}<button class="icon tiny" aria-label={$t('chats.clearSearch')} on:click={() => (query = '')}><Icon name="close" size={13} stroke={2.2} /></button>{/if}
  </div>

  <div class="chan-scroll">
  {#each sections as sec (sec.key)}
   {#if showSec(sec)}
    {#if !sec.loose}
      <!-- svelte-ignore a11y-no-static-element-interactions -->
      <div class="chan-head cat-head" class:over={over === 'sec:' + sec.key || (sec.cat && over === sec.cat.id)} draggable={manage && !!sec.cat}
        on:dragstart={(e) => sec.cat && dragStart(e, sec.cat)} on:dragover={(e) => dragOverSec(e, sec)} on:dragleave={() => (over = '')}
        on:drop={(e) => dropOnSec(e, sec)} on:contextmenu={(e) => edit(e, null, sec)}>
        <button class="cat-fold grow" aria-expanded={!folded.has(sec.key)} on:click={() => fold(sec.key)}>
          <Icon name={folded.has(sec.key) ? 'chevronRight' : 'chevronDown'} size={12} /><span class="nm">{sec.name}</span>
        </button>
        {#if manage}
          <Menu width={200} label={$t('chats.newChannel')} items={[{ id: 'text', label: $t('chats.newChannel'), icon: 'hash' }, { id: 'voice', label: $t('chats.newRoom'), icon: 'volume' }]} on:select={(e) => startAdd(e.detail, sec)}>
            <button slot="trigger" let:toggle class="icon tiny" title={$t('chats.newChannel')} aria-label={$t('chats.newChannel')} aria-haspopup="menu" on:click={toggle}><Icon name="plus" size={14} /></button>
          </Menu>
          <button class="icon tiny" title={$t('chats.categorySettings')} aria-label={$t('chats.categorySettings')} on:click={(e) => edit(e, null, sec)}><Icon name="settings" size={13} /></button>
        {/if}
      </div>
    {/if}
    {#if sec.loose || !folded.has(sec.key)}
      {#each shownItems(sec) as ch (ch.main ? '#main' : ch.id)}
        {#if ch.main}
          <!-- svelte-ignore a11y-no-static-element-interactions -->
          <div class="chan-row" draggable={manage} on:dragstart={(e) => dragStart(e, MAIN)} on:contextmenu={(e) => edit(e, MAIN)}>
            <button class="chan" class:on={$activeChannel === ''} title={$mainChannel.topic || mainName} on:click={openMain}>
              <span class="ci"><Icon name="hash" size={16} /></span>
              <span class="cb"><span class="nm">{mainName}</span>{#if $mainChannel.topic}<span class="pv">{$mainChannel.topic}</span>{/if}</span>
              {#if $mainChannel.readOnly}<span class="ro-ic" title={$t('chats.readOnlyChannel')}><Icon name="lock" size={12} /></span>
              {:else if $mainChannel.slowSecs}<span class="ro-ic" title={$t('chats.slowOn', { s: $mainChannel.slowSecs })}><Icon name="clock" size={12} /></span>{/if}
            </button>
            {#if manage}<button class="icon tiny gear" title={$t('chats.mainSettings')} aria-label={$t('chats.mainSettings')} on:click={(e) => edit(e, MAIN)}><Icon name="settings" size={13} /></button>{/if}
          </div>
        {:else}
          <ChannelRow {ch} {conv} {manage} {inRoom} over={over === ch.id} on:dragstart={(e) => dragStart(e, ch)} on:dragover={(e) => dragOver(e, ch)}
            on:dragleave={() => (over = '')} on:drop={(e) => drop(e, ch)} on:edit={(e) => edit(e.detail, ch)} on:room />
        {/if}
      {:else}
        {#if !sec.loose && adding?.sec?.key !== sec.key}<p class="small faint empty-rooms">{sec.virtual === 'voice' ? $t('chats.roomEmpty') : $t('chats.categoryEmpty')}</p>{/if}
      {/each}
    {/if}
    {#if adding && adding.sec?.key === sec.key}
      <form class="adding" on:submit|preventDefault={add}>
        {#if adding.kind === 'voice'}<Icon name="volume" size={14} />{:else}<span class="hash">#</span>{/if}
        <input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.channelName')} on:keydown={key} on:blur={add} />
      </form>
    {/if}
   {/if}
  {/each}
  {#if (q || filter !== 'all') && !anyShown}<p class="small faint empty-rooms">{$t('layout.nothing')}</p>{/if}
  {#if adding?.kind === 'category'}
    <form class="adding" on:submit|preventDefault={add}><Icon name="folder" size={14} /><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.categoryName')} on:keydown={key} on:blur={add} /></form>
  {/if}
  </div>
  <SelfCard />
</nav>

{#if switching}
  <Popover anchor={switchBtn} width={264} label={$t('layout.switch')} on:close={() => (switching = false)}>
    <div class="switch">
      <button class="sw-item" on:click={() => { switching = false; dispatch('settings'); }}><Icon name="settings" size={17} /><span class="grow nm">{$t('chats.spaceSettings')}</span></button>
      {#if manage}<button class="sw-item" on:click={() => { switching = false; startAdd('category'); }}><Icon name="folder" size={17} /><span class="grow nm">{$t('chats.newCategory')}</span></button>{/if}
      <span class="sw-sep" aria-hidden="true"></span>
      {#each sortedChats as c (c.id)}
        <button class="sw-item" class:sel={c.id === $activeId} on:click={() => pick(c.id)}>
          <Avatar id={c.peerId || c.id} name={c.name} src={c.avatar} size={26} />
          <span class="grow nm">{c.name}</span>
          {#if c.unread}<span class="badge">{c.unread}</span>{/if}
        </button>
      {/each}
      <button class="sw-item all" on:click={allChats}><Icon name="message" size={17} /><span class="grow nm">{$t('layout.allChats')}</span></button>
    </div>
  </Popover>
{/if}

{#if editing}
  <ChannelSettings ed={editing} {cats} {roles} {textChannels} {mainName} groupKind={conv.groupKind} canMove={!editing.main && !editing.sec?.virtual}
    on:save={saveEdit} on:remove={remove} on:move={(e) => move(e.detail)} on:close={() => (editing = null)} />
{/if}

<style>
  .switch { display: flex; flex-direction: column; gap: 1px; padding: 4px; max-height: 340px; overflow-y: auto; }
  .sw-item { display: flex; align-items: center; gap: 10px; width: 100%; padding: 6px 8px; border: none; border-radius: var(--r-md); background: transparent; text-align: left; font-weight: var(--fw-medium); font-size: var(--fs-md); }
  .sw-item:hover:not(:disabled) { background: var(--hover); border: none; }
  .sw-sep { height: 1px; background: var(--border); margin: 4px 0; flex: none; }
  .sw-item.sel { background: var(--surface-3); }
  .sw-item .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sw-item .badge { min-width: 18px; height: 18px; padding: 0 5px; border-radius: var(--r-pill); background: var(--accent); color: var(--accent-ink); font-size: var(--fs-2xs); font-weight: var(--fw-bold); display: inline-grid; place-items: center; }
  .sw-item.all { margin-top: 4px; border-top: 1px solid var(--border); border-radius: 0 0 var(--r-md) var(--r-md); color: var(--muted); padding-top: 9px; }
  .cat-head { margin-top: 10px; border-radius: 8px; }
  .cat-head:first-of-type { margin-top: 2px; }
  .cat-head.over { box-shadow: inset 0 0 0 1px var(--accent); }
  .cat-fold { display: flex; align-items: center; gap: 4px; min-width: 0; padding: 0; background: none; border: 0; color: inherit; font: inherit; text-transform: inherit; cursor: pointer; text-align: left; }
  .cat-fold .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cat-fold:hover { color: var(--text); }
</style>
