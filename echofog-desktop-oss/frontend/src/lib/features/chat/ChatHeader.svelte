<script>
  import { createEventDispatcher } from 'svelte';
  import { isCallActive, stageExpanded, peerInfo } from '../../stores/call.store.js';
  import { t } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';

  /** The chat's head: who or what it is, and its tools (calls, search, info). */
  export let conv;
  export let back = false;
  export let indent = false;
  export let searching = false;
  export let info = false;

  const dispatch = createEventDispatcher();
</script>

<header class:indent>
  {#if back}
    <button class="icon" title={$t('chats.backToChats')} aria-label={$t('chats.backToChats')} on:click={() => dispatch('back')}><Icon name="arrowLeft" size={19} /></button>
  {/if}
  <button class="who" on:click={() => dispatch('info')} title={$t('chats.chatInfo')}>
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
    <button class="icon" title={$t('chats.call')} aria-label={$t('chats.call')} on:click={() => dispatch('call', false)} disabled={$isCallActive || !conv.can.call}><Icon name="phone" size={18} /></button>
    <button class="icon" title={$t('chats.videoCall')} aria-label={$t('chats.videoCall')} on:click={() => dispatch('call', true)} disabled={$isCallActive || !conv.can.call}><Icon name="video" size={19} /></button>
    <button class="icon" class:on={searching} title={$t('chats.searchChat')} aria-label={$t('chats.searchChat')} on:click={() => dispatch('search')}><Icon name="search" size={18} /></button>
    <button class="icon" class:on={info} title={$t('chats.info')} aria-label={$t('chats.info')} on:click={() => dispatch('info')}><Icon name="info" size={19} /></button>
  </div>
</header>
