<script>
  import { createEventDispatcher, tick } from 'svelte';
  import { channels, activeChannel, openChannel, createChannel, removeChannel } from '../../stores/chats.js';
  import { isCallActive, peerInfo } from '../../stores/call.store.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Icon from '../../components/ui/Icon.svelte';

  /** A space's channels and voice rooms: where its conversations and its calls live. Who may manage
   *  the group (conv.can.manage) adds and removes them. */
  export let conv;

  const dispatch = createEventDispatcher();
  $: texts = $channels.filter((c) => !c.voice);
  $: rooms = $channels.filter((c) => c.voice);
  $: inRoom = $isCallActive && $peerInfo.chatId === conv.id ? $peerInfo.room || '' : '';

  let adding = ''; // 'text' | 'voice' while a name is being typed
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
  async function remove(ch) {
    if (!(await ask({ message: $t('chats.confirm.removeChannel', { name: ch.name }), confirm: $t('common.remove'), danger: true }))) return;
    removeChannel(ch.id);
  }
</script>

<nav class="chanbar" aria-label={$t('chats.channels')}>
  <div class="chan-head">
    <span class="eyebrow grow">{$t('chats.channels')}</span>
    {#if conv.can.manage}<button class="icon tiny" title={$t('chats.newChannel')} aria-label={$t('chats.newChannel')} on:click={() => startAdd('text')}><Icon name="plus" size={15} /></button>{/if}
  </div>
  <button class="chan" class:on={$activeChannel === ''} on:click={() => openChannel('')}>
    <span class="hash">#</span><span class="grow nm">{$t('chats.mainChannel')}</span>
  </button>
  {#each texts as ch (ch.id)}
    <div class="chan-row">
      <button class="chan" class:on={$activeChannel === ch.id} class:unread={ch.unread > 0} on:click={() => openChannel(ch.id)}>
        <span class="hash">#</span><span class="grow nm">{ch.name}</span>
        {#if ch.unread > 0 && $activeChannel !== ch.id}<span class="badge">{ch.unread}</span>{/if}
      </button>
      {#if conv.can.manage}<button class="icon tiny rm" title={$t('chats.removeChannel')} aria-label={$t('chats.removeChannel')} on:click={() => remove(ch)}><Icon name="close" size={12} stroke={2.2} /></button>{/if}
    </div>
  {/each}
  {#if adding === 'text'}
    <form class="adding" on:submit|preventDefault={add}><span class="hash">#</span><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.channelName')} on:keydown={key} on:blur={add} /></form>
  {/if}

  <div class="chan-head rooms">
    <span class="eyebrow grow">{$t('chats.voiceRooms')}</span>
    {#if conv.can.manage}<button class="icon tiny" title={$t('chats.newRoom')} aria-label={$t('chats.newRoom')} on:click={() => startAdd('voice')}><Icon name="plus" size={15} /></button>{/if}
  </div>
  {#each rooms as ch (ch.id)}
    <div class="chan-row">
      <button class="chan room" class:live={!!ch.call} class:here={inRoom === ch.id} disabled={!conv.can.call || ($isCallActive && inRoom !== ch.id)}
        title={inRoom === ch.id ? $t('chats.inRoom') : $t('chats.joinRoom')} on:click={() => inRoom !== ch.id && dispatch('room', ch)}>
        <Icon name="phone" size={14} /><span class="grow nm">{ch.name}</span>
        {#if inRoom === ch.id}<span class="small here-note">{$t('chats.inRoom')}</span>
        {:else if ch.call}<span class="pulse"></span><span class="small live-note">{$t('chats.roomLive')}</span>{/if}
      </button>
      {#if conv.can.manage}<button class="icon tiny rm" title={$t('chats.removeChannel')} aria-label={$t('chats.removeChannel')} on:click={() => remove(ch)}><Icon name="close" size={12} stroke={2.2} /></button>{/if}
    </div>
  {:else}
    {#if adding !== 'voice'}<p class="small faint empty-rooms">{$t('chats.roomEmpty')}</p>{/if}
  {/each}
  {#if adding === 'voice'}
    <form class="adding" on:submit|preventDefault={add}><Icon name="phone" size={14} /><input bind:this={input} bind:value={name} maxlength="64" placeholder={$t('chats.channelName')} on:keydown={key} on:blur={add} /></form>
  {/if}
</nav>
