<script>
  import { createEventDispatcher } from 'svelte';
  import { isCallActive, stageExpanded, peerInfo } from '../../stores/call.store.js';
  import { t } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';

  /** The chat's head. A direct chat or a plain group: who or what it is, calls, search, info. A
   *  space: the channel shown and its topic, search, threads and members; its calls live in its
   *  voice rooms, so it has no call buttons once it has one. */
  export let conv;
  export let back = false;
  export let indent = false;
  export let searching = false;
  export let info = false;
  export let threadsOpen = false;
  export let channel = null; // the channel shown in a space: { name, topic } (null: the main one)
  export let hasRooms = false; // the space has voice rooms
  export let channelsButton = false; // a narrow space: its channels open from here

  const dispatch = createEventDispatcher();
  $: space = conv.kind === 'group';
  $: calls = !space || !hasRooms;
</script>

<header class:indent>
  {#if back}
    <button class="icon" title={$t('chats.backToChats')} aria-label={$t('chats.backToChats')} on:click={() => dispatch('back')}><Icon name="arrowLeft" size={19} /></button>
  {/if}
  {#if space && channelsButton}
    <button class="icon" title={$t('chats.channels')} aria-label={$t('chats.channels')} on:click={() => dispatch('channels')}><Icon name="panel" size={18} /></button>
  {/if}
  {#if space}
    <div class="who chan-who">
      <div class="title">
        <h3><span class="chan-title"># {channel ? channel.name : $t('chats.mainChannel')}</span>{#if channel && channel.topic}<span class="topic">{channel.topic}</span>{/if}</h3>
        <p class="small">{conv.name} · {$t('chats.members', { n: conv.members })}</p>
      </div>
    </div>
  {:else}
    <button class="who" on:click={() => dispatch('info')} title={$t('chats.chatInfo')}>
      <div class="av">
        <Avatar id={conv.peerId || conv.id} name={conv.name} src={conv.avatar || ''} size={40} />
        <span class="presence" class:on={conv.online}></span>
      </div>
      <div class="title">
        <h3>{conv.name}</h3>
        <p class="small" class:online={conv.online}>{conv.online ? $t('common.online') : $t('common.offline')}</p>
      </div>
    </button>
  {/if}
  {#if $isCallActive && $peerInfo.chatId === conv.id && !$stageExpanded}
    <button class="primary small with-icon" on:click={() => stageExpanded.set(true)}><Icon name="phone" size={14} /> {$t('chats.showCall')}</button>
  {/if}
  <div class="tools">
    {#if calls}
      <button class="icon" title={$t('chats.call')} aria-label={$t('chats.call')} on:click={() => dispatch('call', false)} disabled={$isCallActive || !conv.can.call}><Icon name="phone" size={18} /></button>
      <button class="icon" title={$t('chats.videoCall')} aria-label={$t('chats.videoCall')} on:click={() => dispatch('call', true)} disabled={$isCallActive || !conv.can.call}><Icon name="video" size={19} /></button>
    {/if}
    <button class="icon" class:on={searching} title={$t('chats.searchChat')} aria-label={$t('chats.searchChat')} on:click={() => dispatch('search')}><Icon name="search" size={18} /></button>
    {#if space}
      <button class="icon" class:on={threadsOpen} title={$t('chats.threads')} aria-label={$t('chats.threads')} on:click={() => dispatch('threads')}><Icon name="message" size={18} /></button>
      <button class="icon" class:on={info} title={$t('chats.membersPanel')} aria-label={$t('chats.membersPanel')} on:click={() => dispatch('info')}><Icon name="users" size={18} /></button>
    {:else}
      <button class="icon" class:on={info} title={$t('chats.info')} aria-label={$t('chats.info')} on:click={() => dispatch('info')}><Icon name="info" size={19} /></button>
    {/if}
  </div>
</header>
