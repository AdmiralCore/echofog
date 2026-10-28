<script>
  import { createEventDispatcher, tick } from 'svelte';
  import { channels, activeChannel, openChannel, createChannel, removeChannel, saveChannel, moveChannel } from '../../stores/chats.js';
  import { isCallActive, peerInfo } from '../../stores/call.store.js';
  import { me } from '../../stores/session.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import Popover from '../../components/ui/Popover.svelte';
  import { useChat } from './context.js';

  /** A space's column: its name and settings, its text channels and its voice rooms with who is in
   *  each. Who may manage the group (conv.can.manage) adds, sets up, reorders and removes them. */
  export let conv;

  const dispatch = createEventDispatcher();
  const { nameOf } = useChat();
  $: texts = $channels.filter((c) => !c.voice);
  $: rooms = $channels.filter((c) => c.voice);
  $: inRoom = $isCallActive && $peerInfo.chatId === conv.id ? $peerInfo.room || '' : '';
  $: manage = !!conv.can.manage;

  // ---- a new channel: its name typed in place ----------------------------------------------------
  let adding = ''; // 'text' | 'voice'
  let name = '';
  let input;
  async function startAdd(kind) {
    adding = kind;
    name = '';
    await tick();
    input?.focus();
  }
  // Enter and leaving the field both end the typing: whichever comes first makes the channel
  async function add() {
    const kind = adding;
    const n = name.trim();
    adding = '';
    name = '';
    if (!kind || !n) return;
    const ch = await createChannel(n, kind === 'voice');
    if (ch && !ch.voice) openChannel(ch.id);
  }
  function key(e) {
    if (e.key === 'Escape') { e.preventDefault(); adding = ''; }
  }

  // ---- a channel's settings ----------------------------------------------------------------------
  let editing = null; // { ch, anchor, name, topic, readOnly }
  function edit(e, ch) {
    e.preventDefault();
    e.stopPropagation();
    if (!manage) return;
    editing = { ch, anchor: e.currentTarget.closest('.chan-row') || e.currentTarget, name: ch.name, topic: ch.topic || '', readOnly: !!ch.readOnly };
  }
  async function saveEdit() {
    const ed = editing;
    if (!ed.name.trim()) return;
    editing = null;
    await saveChannel({ ...ed.ch, name: ed.name, topic: ed.topic, readOnly: ed.readOnly });
  }
  function move(dir) {
    const ch = editing.ch;
    const list = ch.voice ? rooms : texts;
    const i = list.findIndex((c) => c.id === ch.id);
    editing = null;
    if (i + dir >= 0 && i + dir < list.length) moveChannel(ch.id, i + dir);
  }
  async function remove() {
    const ch = editing.ch;
    editing = null;
    if (!(await ask({ message: $t('chats.confirm.removeChannel', { name: ch.name }), confirm: $t('common.remove'), danger: true }))) return;
    removeChannel(ch.id);
  }

  // ---- reordering by dragging --------------------------------------------------------------------
  let dragged = null;
  let over = '';
  function dragStart(e, ch) {
    if (!manage) return;
    dragged = ch;
    e.dataTransfer.effectAllowed = 'move';
  }
  function dragOver(e, ch) {
    if (!dragged || dragged.voice !== ch.voice) return;
    e.preventDefault();
    over = ch.id;
  }
  function drop(e, ch) {
    e.preventDefault();
    const from = dragged;
    dragged = null;
    over = '';
    if (!from || from.id === ch.id || from.voice !== ch.voice) return;
    const list = from.voice ? rooms : texts;
    moveChannel(from.id, list.findIndex((c) => c.id === ch.id));
  }
</script>

<nav class="chanbar" aria-label={$t('chats.channels')}>
  <div class="space-head">
    <span class="space-name grow">{conv.name}</span>
    <button class="icon tiny" title={$t('chats.spaceSettings')} aria-label={$t('chats.spaceSettings')} on:click={() => dispatch('settings')}><Icon name="settings" size={16} /></button>
  </div>

  <div class="chan-head">
    <span class="grow">{$t('chats.text')}</span>
    {#if manage}<button class="icon tiny" title={$t('chats.newChannel')} aria-label={$t('chats.newChannel')} on:click={() => startAdd('text')}><Icon name="plus" size={15} /></button>{/if}
  </div>
  <div class="chan-row">
    <button class="chan" class:on={$activeChannel === ''} on:click={() => openChannel('')}>
      <span class="hash">#</span><span class="grow nm">{$t('chats.mainChannel')}</span>
    </button>
  </div>
  {#each texts as ch (ch.id)}
    <!-- svelte-ignore a11y-no-static-element-interactions -->
    <div class="chan-row" class:over={over === ch.id} draggable={manage} on:dragstart={(e) => dragStart(e, ch)} on:dragover={(e) => dragOver(e, ch)}
      on:dragleave={() => (over = '')} on:drop={(e) => drop(e, ch)} on:contextmenu={(e) => edit(e, ch)}>
      <button class="chan" class:on={$activeChannel === ch.id} class:unread={ch.unread > 0} title={ch.topic || ch.name} on:click={() => openChannel(ch.id)}>
        <span class="hash">#</span><span class="grow nm">{ch.name}</span>
        {#if ch.unread > 0 && $activeChannel !== ch.id}<span class="badge">{ch.unread}</span>
        {:else if ch.readOnly}<span class="ro-ic" title={$t('chats.readOnlyChannel')}><Icon name="lock" size={12} /></span>{/if}
      </button>
      {#if manage}<button class="icon tiny gear" title={$t('chats.channelSettings')} aria-label={$t('chats.channelSettings')} on:click={(e) => edit(e, ch)}><Icon name="settings" size={13} /></button>{/if}
    </div>
  {/each}
  {#if adding === 'text'}
    <form class="adding" on:submit|preventDefault={add}><span class="hash">#</span><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.channelName')} on:keydown={key} on:blur={add} /></form>
  {/if}

  <div class="chan-head rooms">
    <span class="grow">{$t('chats.voice')}</span>
    {#if manage}<button class="icon tiny" title={$t('chats.newRoom')} aria-label={$t('chats.newRoom')} on:click={() => startAdd('voice')}><Icon name="plus" size={15} /></button>{/if}
  </div>
  {#each rooms as ch (ch.id)}
    <!-- svelte-ignore a11y-no-static-element-interactions -->
    <div class="chan-row" class:over={over === ch.id} draggable={manage} on:dragstart={(e) => dragStart(e, ch)} on:dragover={(e) => dragOver(e, ch)}
      on:dragleave={() => (over = '')} on:drop={(e) => drop(e, ch)} on:contextmenu={(e) => edit(e, ch)}>
      <button class="chan room" class:here={inRoom === ch.id} disabled={!conv.can.call || ($isCallActive && inRoom !== ch.id)}
        title={inRoom === ch.id ? $t('chats.inRoom') : $t('chats.joinRoom')} on:click={() => inRoom !== ch.id && dispatch('room', ch)}>
        <span class="vol"><Icon name="volume" size={15} /></span><span class="grow nm">{ch.name}</span>
      </button>
      {#if manage}<button class="icon tiny gear" title={$t('chats.channelSettings')} aria-label={$t('chats.channelSettings')} on:click={(e) => edit(e, ch)}><Icon name="settings" size={13} /></button>{/if}
    </div>
    {#if ch.call && ch.call.members && ch.call.members.length}
      <div class="in-room">
        {#each ch.call.members as id (id)}
          <div class="who-in"><Avatar {id} name={id === $me.id ? $t('chats.You') : $nameOf(id)} size={18} /><span class="nm">{id === $me.id ? $t('chats.You') : $nameOf(id)}</span></div>
        {/each}
      </div>
    {/if}
  {:else}
    {#if adding !== 'voice'}<p class="small faint empty-rooms">{$t('chats.roomEmpty')}</p>{/if}
  {/each}
  {#if adding === 'voice'}
    <form class="adding" on:submit|preventDefault={add}><Icon name="volume" size={14} /><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.channelName')} on:keydown={key} on:blur={add} /></form>
  {/if}
</nav>

{#if editing}
  <Popover anchor={editing.anchor} width={280} label={$t('chats.channelSettings')} on:close={() => (editing = null)}>
    <form class="ch-edit" on:submit|preventDefault={saveEdit}>
      <label class="small faint" for="ch-name">{$t('chats.channelName')}</label>
      <input id="ch-name" bind:value={editing.name} maxlength="64" />
      {#if !editing.ch.voice}
        <label class="small faint" for="ch-topic">{$t('chats.channelTopic')}</label>
        <input id="ch-topic" bind:value={editing.topic} maxlength="256" />
        <label class="check"><input type="checkbox" bind:checked={editing.readOnly} /> <span>{$t('chats.readOnlyChannel')}</span></label>
        <p class="small faint">{$t('chats.readOnlyHint')}</p>
      {/if}
      <div class="row ch-actions">
        <button type="button" class="icon" title={$t('chats.moveUp')} aria-label={$t('chats.moveUp')} on:click={() => move(-1)}><Icon name="chevronUp" size={16} /></button>
        <button type="button" class="icon" title={$t('chats.moveDown')} aria-label={$t('chats.moveDown')} on:click={() => move(1)}><Icon name="chevronDown" size={16} /></button>
        <span class="grow"></span>
        <button type="button" class="danger small" on:click={remove}>{$t('common.remove')}</button>
        <button type="submit" class="primary small" disabled={!editing.name.trim()}>{$t('common.save')}</button>
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
</style>
