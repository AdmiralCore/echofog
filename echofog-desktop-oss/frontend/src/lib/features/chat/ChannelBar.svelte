<script>
  import { createEventDispatcher, tick } from 'svelte';
  import { channels, activeChannel, mainChannel, openChannel, createChannel, createCategory, removeChannel, saveChannel, saveMainChannel, moveChannel } from '../../stores/chats.js';
  import { isCallActive, peerInfo } from '../../stores/call.store.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Icon from '../../components/ui/Icon.svelte';
  import Popover from '../../components/ui/Popover.svelte';
  import ChannelRow from './ChannelRow.svelte';

  /** A space's column: its name and settings, its main channel, the text channels and voice rooms
   *  outside any category, then each category with its own. Who may manage channels
   *  (conv.can.channels) adds, sets up, reorders, files and removes them. */
  export let conv;

  const dispatch = createEventDispatcher();
  const SLOW = [0, 5, 10, 30, 60, 300, 900, 3600];
  const kind = (c) => (c.category ? 2 : c.voice ? 1 : 0);

  $: cats = $channels.filter((c) => c.category);
  $: catIds = new Set(cats.map((c) => c.id));
  $: parentOf = (c) => (c.parent && catIds.has(c.parent) ? c.parent : '');
  $: texts = $channels.filter((c) => !c.voice && !c.category && !parentOf(c));
  $: rooms = $channels.filter((c) => c.voice && !parentOf(c));
  $: inCat = (id) => [...$channels.filter((c) => !c.voice && !c.category && parentOf(c) === id), ...$channels.filter((c) => c.voice && parentOf(c) === id)];
  $: textChannels = $channels.filter((c) => !c.voice && !c.category);
  $: inRoom = $isCallActive && $peerInfo.chatId === conv.id ? $peerInfo.room || '' : '';
  $: manage = !!(conv.can.channels ?? conv.can.manage);
  $: mainName = $mainChannel.name || $t('chats.mainChannel');

  // ---- folded categories, remembered per space on this device ------------------------------------
  const foldKey = () => 'ef.chanfold.' + conv.id;
  let folded = new Set();
  $: conv.id, loadFolded();
  function loadFolded() {
    try { folded = new Set(JSON.parse(localStorage.getItem(foldKey()) || '[]')); } catch (_) { folded = new Set(); }
  }
  function fold(id) {
    folded.has(id) ? folded.delete(id) : folded.add(id);
    folded = folded;
    try { localStorage.setItem(foldKey(), JSON.stringify([...folded])); } catch (_) { /* not kept */ }
  }

  // ---- a new channel, room or category: its name typed in place ----------------------------------
  let adding = null; // { kind: 'text' | 'voice' | 'category', parent }
  let name = '';
  let input;
  async function startAdd(k, parent = '') {
    adding = { kind: k, parent };
    name = '';
    if (parent && folded.has(parent)) fold(parent);
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
    const ch = await createChannel(n, a.kind === 'voice', a.parent);
    if (ch && !ch.voice) openChannel(ch.id);
  }
  function key(e) {
    if (e.key === 'Escape') { e.preventDefault(); adding = null; }
  }

  // ---- settings of a channel, a category or the main channel -------------------------------------
  let editing = null; // { ch, main, anchor, name, topic, readOnly, slowSecs, parent, open }
  function edit(e, ch, main = false) {
    e.preventDefault();
    e.stopPropagation();
    if (!manage) return;
    const src = main ? $mainChannel : ch;
    editing = { ch, main, anchor: e.currentTarget.closest('.chan-row, .cat-head') || e.currentTarget, name: main ? src.name || '' : src.name,
      topic: src.topic || '', readOnly: !!src.readOnly, slowSecs: src.slowSecs || 0, parent: main ? '' : parentOf(src), open: main ? src.open || '' : '' };
  }
  async function saveEdit() {
    const ed = editing;
    if (!ed.main && !ed.name.trim()) return;
    editing = null;
    if (ed.main) await saveMainChannel(ed);
    else await saveChannel({ ...ed.ch, name: ed.name, topic: ed.topic, readOnly: ed.readOnly, slowSecs: ed.slowSecs, parent: ed.parent });
  }
  // the channels are ordered among their kind; up and down move past the neighbour shown above or below
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
    const ch = editing.ch;
    editing = null;
    const msg = ch.category ? 'chats.confirm.removeCategory' : 'chats.confirm.removeChannel';
    if (!(await ask({ message: $t(msg, { name: ch.name }), confirm: $t('common.remove'), danger: true }))) return;
    removeChannel(ch.id);
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
    if (!dragged || kind(dragged) !== kind(ch)) return;
    e.preventDefault();
    over = ch.id;
  }
  function dragOverCat(e, cat) {
    if (!dragged || dragged.category) return dragOver(e, cat);
    e.preventDefault();
    over = 'cat:' + cat.id;
  }
  async function drop(e, ch) {
    e.preventDefault();
    const from = dragged;
    dragged = null;
    over = '';
    if (!from || from.id === ch.id || kind(from) !== kind(ch)) return;
    if (!from.category && parentOf(from) !== parentOf(ch)) await saveChannel({ ...from, parent: parentOf(ch) });
    const rest = $channels.filter((c) => kind(c) === kind(from) && c.id !== from.id);
    moveChannel(from.id, rest.findIndex((c) => c.id === ch.id));
  }
  function dropOnCat(e, cat) {
    if (!dragged || dragged.category) return drop(e, cat);
    e.preventDefault();
    const from = dragged;
    dragged = null;
    over = '';
    if (parentOf(from) !== cat.id) saveChannel({ ...from, parent: cat.id });
  }
</script>

<nav class="chanbar" aria-label={$t('chats.channels')}>
  <div class="space-head">
    <span class="space-name grow">{conv.name}</span>
    {#if manage}<button class="icon tiny" title={$t('chats.newCategory')} aria-label={$t('chats.newCategory')} on:click={() => startAdd('category')}><Icon name="folder" size={15} /></button>{/if}
    <button class="icon tiny" title={$t('chats.spaceSettings')} aria-label={$t('chats.spaceSettings')} on:click={() => dispatch('settings')}><Icon name="settings" size={16} /></button>
  </div>

  <div class="chan-head">
    <span class="grow">{$t('chats.text')}</span>
    {#if manage}<button class="icon tiny" title={$t('chats.newChannel')} aria-label={$t('chats.newChannel')} on:click={() => startAdd('text')}><Icon name="plus" size={15} /></button>{/if}
  </div>
  <!-- svelte-ignore a11y-no-static-element-interactions -->
  <div class="chan-row" on:contextmenu={(e) => edit(e, null, true)}>
    <button class="chan" class:on={$activeChannel === ''} title={$mainChannel.topic || mainName} on:click={() => openChannel('')}>
      <span class="hash">#</span><span class="grow nm">{mainName}</span>
      {#if $mainChannel.readOnly}<span class="ro-ic" title={$t('chats.readOnlyChannel')}><Icon name="lock" size={12} /></span>
      {:else if $mainChannel.slowSecs}<span class="ro-ic" title={$t('chats.slowOn', { s: $mainChannel.slowSecs })}><Icon name="clock" size={12} /></span>{/if}
    </button>
    {#if manage}<button class="icon tiny gear" title={$t('chats.mainSettings')} aria-label={$t('chats.mainSettings')} on:click={(e) => edit(e, null, true)}><Icon name="settings" size={13} /></button>{/if}
  </div>
  {#each texts as ch (ch.id)}
    <ChannelRow {ch} {conv} {manage} {inRoom} over={over === ch.id} on:dragstart={(e) => dragStart(e, ch)} on:dragover={(e) => dragOver(e, ch)}
      on:dragleave={() => (over = '')} on:drop={(e) => drop(e, ch)} on:edit={(e) => edit(e.detail, ch)} on:room />
  {/each}
  {#if adding?.kind === 'text' && !adding.parent}
    <form class="adding" on:submit|preventDefault={add}><span class="hash">#</span><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.channelName')} on:keydown={key} on:blur={add} /></form>
  {/if}

  <div class="chan-head rooms">
    <span class="grow">{$t('chats.voice')}</span>
    {#if manage}<button class="icon tiny" title={$t('chats.newRoom')} aria-label={$t('chats.newRoom')} on:click={() => startAdd('voice')}><Icon name="plus" size={15} /></button>{/if}
  </div>
  {#each rooms as ch (ch.id)}
    <ChannelRow {ch} {conv} {manage} {inRoom} over={over === ch.id} on:dragstart={(e) => dragStart(e, ch)} on:dragover={(e) => dragOver(e, ch)}
      on:dragleave={() => (over = '')} on:drop={(e) => drop(e, ch)} on:edit={(e) => edit(e.detail, ch)} on:room />
  {:else}
    {#if !(adding?.kind === 'voice' && !adding.parent)}<p class="small faint empty-rooms">{$t('chats.roomEmpty')}</p>{/if}
  {/each}
  {#if adding?.kind === 'voice' && !adding.parent}
    <form class="adding" on:submit|preventDefault={add}><Icon name="volume" size={14} /><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.channelName')} on:keydown={key} on:blur={add} /></form>
  {/if}

  {#each cats as cat (cat.id)}
    <!-- svelte-ignore a11y-no-static-element-interactions -->
    <div class="chan-head cat-head" class:over={over === cat.id || over === 'cat:' + cat.id} draggable={manage} on:dragstart={(e) => dragStart(e, cat)}
      on:dragover={(e) => dragOverCat(e, cat)} on:dragleave={() => (over = '')} on:drop={(e) => dropOnCat(e, cat)} on:contextmenu={(e) => edit(e, cat)}>
      <button class="cat-fold grow" aria-expanded={!folded.has(cat.id)} on:click={() => fold(cat.id)}>
        <Icon name={folded.has(cat.id) ? 'chevronRight' : 'chevronDown'} size={12} /><span class="nm">{cat.name}</span>
      </button>
      {#if manage}
        <button class="icon tiny" title={$t('chats.newChannel')} aria-label={$t('chats.newChannel')} on:click={() => startAdd('text', cat.id)}><Icon name="plus" size={14} /></button>
        <button class="icon tiny" title={$t('chats.newRoom')} aria-label={$t('chats.newRoom')} on:click={() => startAdd('voice', cat.id)}><Icon name="volume" size={13} /></button>
        <button class="icon tiny" title={$t('chats.categorySettings')} aria-label={$t('chats.categorySettings')} on:click={(e) => edit(e, cat)}><Icon name="settings" size={13} /></button>
      {/if}
    </div>
    {#if !folded.has(cat.id)}
      {#each inCat(cat.id) as ch (ch.id)}
        <ChannelRow {ch} {conv} {manage} {inRoom} over={over === ch.id} on:dragstart={(e) => dragStart(e, ch)} on:dragover={(e) => dragOver(e, ch)}
          on:dragleave={() => (over = '')} on:drop={(e) => drop(e, ch)} on:edit={(e) => edit(e.detail, ch)} on:room />
      {:else}
        {#if adding?.parent !== cat.id}<p class="small faint empty-rooms">{$t('chats.categoryEmpty')}</p>{/if}
      {/each}
    {/if}
    {#if adding && adding.parent === cat.id}
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
  <Popover anchor={editing.anchor} width={290} label={editing.main ? $t('chats.mainSettings') : editing.ch.category ? $t('chats.categorySettings') : $t('chats.channelSettings')} on:close={() => (editing = null)}>
    <form class="ch-edit" on:submit|preventDefault={saveEdit}>
      <label class="small faint" for="ch-name">{editing.ch?.category ? $t('chats.categoryName') : $t('chats.channelName')}</label>
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
      {#if !editing.main && !editing.ch.category && cats.length}
        <label class="small faint" for="ch-cat">{$t('chats.category')}</label>
        <select id="ch-cat" bind:value={editing.parent}>
          <option value="">{$t('chats.noCategory')}</option>
          {#each cats as c (c.id)}<option value={c.id}>{c.name}</option>{/each}
        </select>
      {/if}
      {#if editing.main}
        <label class="small faint" for="ch-open">{$t('chats.openOn')}</label>
        <select id="ch-open" bind:value={editing.open}>
          <option value="">{mainName}</option>
          {#each textChannels as c (c.id)}<option value={c.id}>#{c.name}</option>{/each}
        </select>
      {/if}
      <div class="row ch-actions">
        {#if !editing.main}
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
  .cat-head.over { box-shadow: inset 0 0 0 1px var(--accent); }
  .cat-fold { display: flex; align-items: center; gap: 4px; min-width: 0; padding: 0; background: none; border: 0; color: inherit; font: inherit; text-transform: inherit; cursor: pointer; text-align: left; }
  .cat-fold .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cat-fold:hover { color: var(--text); }
</style>
