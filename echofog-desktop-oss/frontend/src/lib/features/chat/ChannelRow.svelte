<script>
  import { createEventDispatcher } from 'svelte';
  import { activeChannel, openChannel } from '../../stores/chats.js';
  import { isCallActive } from '../../stores/call.store.js';
  import { me } from '../../stores/session.js';
  import { t } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import { useChat } from './context.js';

  /** One text channel or voice room of a space's column (ChannelBar), with who is in a room. The
   *  column handles its settings (edit) and the dragging. */
  export let ch;
  export let conv;
  export let manage = false;
  export let inRoom = '';
  export let over = false;

  const dispatch = createEventDispatcher();
  const { nameOf } = useChat();
</script>

<!-- svelte-ignore a11y-no-static-element-interactions -->
<div class="chan-row" class:over draggable={manage} on:dragstart on:dragover on:dragleave on:drop on:contextmenu={(e) => dispatch('edit', e)}>
  {#if ch.voice}
    <button class="chan room" class:here={inRoom === ch.id} disabled={!(ch.canJoin ?? conv.can.call) || ($isCallActive && inRoom !== ch.id)}
      title={inRoom === ch.id ? $t('chats.inRoom') : $t('chats.joinRoom')} on:click={() => inRoom !== ch.id && dispatch('room', ch)}>
      <span class="vol"><Icon name="volume" size={15} /></span><span class="grow nm">{ch.name}</span>
    </button>
  {:else}
    <button class="chan" class:on={$activeChannel === ch.id} class:unread={ch.unread > 0} title={ch.topic || ch.name} on:click={() => openChannel(ch.id)}>
      <span class="hash">#</span><span class="grow nm">{ch.name}</span>
      {#if ch.unread > 0 && $activeChannel !== ch.id}<span class="badge">{ch.unread}</span>
      {:else if ch.readOnly}<span class="ro-ic" title={$t('chats.readOnlyChannel')}><Icon name="lock" size={12} /></span>
      {:else if ch.slowSecs}<span class="ro-ic" title={$t('chats.slowOn', { s: ch.slowSecs })}><Icon name="clock" size={12} /></span>{/if}
    </button>
  {/if}
  {#if manage}<button class="icon tiny gear" title={$t('chats.channelSettings')} aria-label={$t('chats.channelSettings')} on:click={(e) => dispatch('edit', e)}><Icon name="settings" size={13} /></button>{/if}
</div>
{#if ch.voice && ch.call && ch.call.members && ch.call.members.length}
  <div class="in-room">
    {#each ch.call.members as id (id)}
      <div class="who-in"><Avatar {id} name={id === $me.id ? $t('chats.You') : $nameOf(id)} size={18} /><span class="nm">{id === $me.id ? $t('chats.You') : $nameOf(id)}</span></div>
    {/each}
  </div>
{/if}
