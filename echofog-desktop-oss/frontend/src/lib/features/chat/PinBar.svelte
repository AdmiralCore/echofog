<script>
  import { slide } from 'svelte/transition';
  import { pins, pinMessage, preview } from '../../stores/chats.js';
  import { t } from '../../i18n/index.js';
  import Icon from '../../components/ui/Icon.svelte';
  import { useChat } from './context.js';

  /** The newest pin; a click goes to it and moves on to the one pinned before it. */
  export let chatId = '';
  export let canPin = false;

  const chat = useChat();
  let idx = 0;
  $: chatId, (idx = 0); // another chat starts at its newest pin
  $: if (idx >= $pins.length) idx = 0;
  $: shown = $pins[idx] || null;

  function next() {
    if (!shown) return;
    chat.goTo(shown.id);
    idx = (idx + 1) % $pins.length;
  }
</script>

{#if shown}
  <button class="pinbar" on:click={next} title={$t('chats.goToPin')} transition:slide={{ duration: 180 }}>
    <span class="pin-ic"><Icon name="pin" size={15} /></span>
    <span class="grow pin-body">
      <span class="pin-head">{$t('chats.pinned')}{#if $pins.length > 1} · {idx + 1}/{$pins.length}{/if}</span>
      <span class="pin-text">{preview(shown)}</span>
    </span>
    {#if canPin}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <span class="icon tiny unpin" title={$t('chats.unpin')} on:click|stopPropagation={() => pinMessage(shown.id, false)}><Icon name="close" size={13} stroke={2.2} /></span>
    {/if}
  </button>
{/if}
