<script>
  import { createEventDispatcher, tick } from 'svelte';
  import { channels, activeChannel, mainChannel, openChannel, createChannel, createCategory, removeChannel, saveChannel, saveMainChannel, moveChannel, placeChannel, setupCategories } from '../../stores/chats.js';
  import { isCallActive, peerInfo } from '../../stores/call.store.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Icon from '../../components/ui/Icon.svelte';
  import Popover from '../../components/ui/Popover.svelte';
  import ChannelRow from './ChannelRow.svelte';
  import ChannelPerms from './ChannelPerms.svelte';
  import { App } from '../../api.js';

  /** A space's column: its name and settings, then its categories, each with text channels and
   *  voice rooms (the main channel among them). A space without categories shows two headings,
   *  text and voice; the first change to either (a rename, a removal, a channel added to it) makes
   *  them real categories (ChatCategoriesSetup). Channels in no category sit on top. Who may manage
   *  channels (conv.can.channels) adds, sets up, reorders, files and removes them. */
  export let conv;

  const dispatch = createEventDispatcher();
  const SLOW = [0, 5, 10, 30, 60, 300, 900, 3600];
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
        { key: 'v:text', virtual: 'text', name: $t('chats.text'), items: [MAIN, ...$channels.filter((c) => !c.voice && !c.category)] },
        { key: 'v:voice', virtual: 'voice', name: $t('chats.voice'), items: $channels.filter((c) => c.voice) },
      ];
  $: textChannels = $channels.filter((c) => !c.voice && !c.category);
  $: inRoom = $isCallActive && $peerInfo.chatId === conv.id ? $peerInfo.room || '' : '';
  $: manage = !!(conv.can.channels ?? conv.can.manage);
  $: mainName = $mainChannel.name || $t('chats.mainChannel');

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
  let editing = null; // { ch, sec, main, anchor, name, topic, readOnly, slowSecs, parent, open, overrides }
  let roles = []; // the group's roles but the owner's, for a channel's permissions
  async function loadRoles() {
    try { roles = ((await App.GroupRoles(conv.groupId)) || []).filter((r) => r.rank < 100); } catch (_) { roles = []; }
  }
  function edit(e, ch, sec = null) {
    e.preventDefault();
    e.stopPropagation();
    if (!manage) return;
    const anchor = e.currentTarget.closest('.chan-row, .cat-head') || e.currentTarget;
    if (sec) return (editing = { sec, ch: sec.cat || { category: true }, anchor, name: sec.name });
    const main = !!ch.main;
    const src = main ? $mainChannel : ch;
    editing = { ch, main, anchor, name: main ? src.name || '' : src.name, topic: src.topic || '', readOnly: !!src.readOnly,
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
  const slowLabel = (s) => (!s ? $t('chats.slowOff') : s < 60 ? $t('chats.slowSec', { n: s }) : s < 3600 ? $t('chats.slowMin', { n: s / 60 }) : $t('chats.slowHour', { n: s / 3600 }));

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
    <span class="space-name grow">{conv.name}</span>
    {#if manage}<button class="icon tiny" title={$t('chats.newCategory')} aria-label={$t('chats.newCategory')} on:click={() => startAdd('category')}><Icon name="folder" size={15} /></button>{/if}
    <button class="icon tiny" title={$t('chats.spaceSettings')} aria-label={$t('chats.spaceSettings')} on:click={() => dispatch('settings')}><Icon name="settings" size={16} /></button>
  </div>

  {#each sections as sec (sec.key)}
    {#if !sec.loose}
      <!-- svelte-ignore a11y-no-static-element-interactions -->
      <div class="chan-head cat-head" class:over={over === 'sec:' + sec.key || (sec.cat && over === sec.cat.id)} draggable={manage && !!sec.cat}
        on:dragstart={(e) => sec.cat && dragStart(e, sec.cat)} on:dragover={(e) => dragOverSec(e, sec)} on:dragleave={() => (over = '')}
        on:drop={(e) => dropOnSec(e, sec)} on:contextmenu={(e) => edit(e, null, sec)}>
        <button class="cat-fold grow" aria-expanded={!folded.has(sec.key)} on:click={() => fold(sec.key)}>
          <Icon name={folded.has(sec.key) ? 'chevronRight' : 'chevronDown'} size={12} /><span class="nm">{sec.name}</span>
        </button>
        {#if manage}
          <button class="icon tiny" title={$t('chats.newChannel')} aria-label={$t('chats.newChannel')} on:click={() => startAdd('text', sec)}><Icon name="plus" size={14} /></button>
          <button class="icon tiny" title={$t('chats.newRoom')} aria-label={$t('chats.newRoom')} on:click={() => startAdd('voice', sec)}><Icon name="volume" size={13} /></button>
          <button class="icon tiny" title={$t('chats.categorySettings')} aria-label={$t('chats.categorySettings')} on:click={(e) => edit(e, null, sec)}><Icon name="settings" size={13} /></button>
        {/if}
      </div>
    {/if}
    {#if sec.loose || !folded.has(sec.key)}
      {#each sec.items as ch (ch.main ? '#main' : ch.id)}
        {#if ch.main}
          <!-- svelte-ignore a11y-no-static-element-interactions -->
          <div class="chan-row" draggable={manage} on:dragstart={(e) => dragStart(e, MAIN)} on:contextmenu={(e) => edit(e, MAIN)}>
            <button class="chan" class:on={$activeChannel === ''} title={$mainChannel.topic || mainName} on:click={() => openChannel('')}>
              <span class="hash">#</span><span class="grow nm">{mainName}</span>
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
  {/each}
  {#if adding?.kind === 'category'}
    <form class="adding" on:submit|preventDefault={add}><Icon name="folder" size={14} /><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.categoryName')} on:keydown={key} on:blur={add} /></form>
  {/if}
</nav>

{#if editing}
  <Popover anchor={editing.anchor} width={editing.ch.category ? 290 : 330} label={editing.main ? $t('chats.mainSettings') : editing.ch.category ? $t('chats.categorySettings') : $t('chats.channelSettings')} on:close={() => (editing = null)}>
    <form class="ch-edit" on:submit|preventDefault={saveEdit}>
      <label class="small faint" for="ch-name">{editing.ch.category ? $t('chats.categoryName') : $t('chats.channelName')}</label>
      <input id="ch-name" bind:value={editing.name} maxlength="64" placeholder={editing.main ? $t('chats.mainChannel') : ''} />
      {#if editing.main || (!editing.ch.voice && !editing.ch.category)}
        <label class="small faint" for="ch-topic">{$t('chats.channelTopic')}</label>
        <input id="ch-topic" bind:value={editing.topic} maxlength="256" />
        <label class="small faint" for="ch-slow">{$t('chats.slowMode')}</label>
        <select id="ch-slow" bind:value={editing.slowSecs}>
          {#each SLOW as s}<option value={s}>{slowLabel(s)}</option>{/each}
        </select>
        <label class="check"><input type="checkbox" bind:checked={editing.readOnly} /> <span>{$t('chats.readOnlyChannel')}</span></label>
        <p class="small faint">{$t('chats.readOnlyHint')}</p>
      {/if}
      {#if !editing.ch.category && cats.length}
        <label class="small faint" for="ch-cat">{$t('chats.category')}</label>
        <select id="ch-cat" bind:value={editing.parent}>
          <option value="">{$t('chats.noCategory')}</option>
          {#each cats as c (c.id)}<option value={c.id}>{c.name}</option>{/each}
        </select>
      {/if}
      {#if !editing.ch.category && conv.groupKind !== 'channel'}
        <ChannelPerms bind:overrides={editing.overrides} {roles} voice={!!editing.ch.voice} />
      {/if}
      {#if editing.main}
        <label class="small faint" for="ch-open">{$t('chats.openOn')}</label>
        <select id="ch-open" bind:value={editing.open}>
          <option value="">{mainName}</option>
          {#each textChannels as c (c.id)}<option value={c.id}>#{c.name}</option>{/each}
        </select>
      {/if}
      <div class="row ch-actions">
        {#if !editing.main && !editing.sec?.virtual}
          <button type="button" class="icon" title={$t('chats.moveUp')} aria-label={$t('chats.moveUp')} on:click={() => move(-1)}><Icon name="chevronUp" size={16} /></button>
          <button type="button" class="icon" title={$t('chats.moveDown')} aria-label={$t('chats.moveDown')} on:click={() => move(1)}><Icon name="chevronDown" size={16} /></button>
        {/if}
        <span class="grow"></span>
        {#if !editing.main}<button type="button" class="danger small" on:click={remove}>{$t('common.remove')}</button>{/if}
        <button type="submit" class="primary small" disabled={!editing.main && !editing.name.trim()}>{$t('common.save')}</button>
      </div>
    </form>
  </Popover>
{/if}

<style>
  .ch-edit { display: flex; flex-direction: column; gap: 6px; padding: 12px; }
  .ch-edit label.small { margin-top: 4px; }
  .check { display: flex; align-items: center; gap: 8px; margin-top: 8px; font-size: 13px; }
  .check input { width: auto; }
  .ch-actions { margin-top: 8px; gap: 4px; align-items: center; }
  .cat-head { margin-top: 10px; border-radius: 8px; }
  .cat-head:first-of-type { margin-top: 2px; }
  .cat-head.over { box-shadow: inset 0 0 0 1px var(--accent); }
  .cat-fold { display: flex; align-items: center; gap: 4px; min-width: 0; padding: 0; background: none; border: 0; color: inherit; font: inherit; text-transform: inherit; cursor: pointer; text-align: left; }
  .cat-fold .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cat-fold:hover { color: var(--text); }
</style>
