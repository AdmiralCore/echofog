<script>
  import { createEventDispatcher, onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import { t } from '../../i18n/index.js';
  import Icon from '../../components/ui/Icon.svelte';

  /** Search inside the chat: the query, and where among the matches the reader is. */
  export let query = '';
  export let count = 0; // matches
  export let index = -1; // the current one, 0-based
  export let active = false; // a query is typed

  const dispatch = createEventDispatcher();
  let input;
  onMount(() => { input?.focus(); input?.select(); });

  /** Puts the cursor back in the field (Ctrl+F while it is open). */
  export function focus() { input?.focus(); input?.select(); }

  function key(e) {
    if (e.key === 'Enter') { e.preventDefault(); dispatch('step', e.shiftKey ? 1 : -1); }
    else if (e.key === 'Escape') { e.preventDefault(); dispatch('close'); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); dispatch('step', -1); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); dispatch('step', 1); }
  }
</script>

<div class="findbar" transition:slide={{ duration: 180 }}>
  <Icon name="search" size={15} />
  <input bind:this={input} bind:value={query} on:keydown={key} placeholder={$t('chats.searchPlaceholder')} spellcheck="false" aria-label={$t('chats.searchChat')} />
  <span class="count small" class:none={active && !count}>{#if active}{count ? $t('chats.matchOf', { i: index + 1, n: count }) : $t('chats.noMatches')}{/if}</span>
  <button class="icon tiny" title={$t('chats.prevMatch')} aria-label={$t('chats.prevMatch')} disabled={!count} on:click={() => dispatch('step', -1)}><Icon name="chevronUp" size={16} /></button>
  <button class="icon tiny" title={$t('chats.nextMatch')} aria-label={$t('chats.nextMatch')} disabled={!count} on:click={() => dispatch('step', 1)}><Icon name="chevronDown" size={16} /></button>
  <button class="icon tiny" title={$t('common.close')} aria-label={$t('common.close')} on:click={() => dispatch('close')}><Icon name="close" size={14} stroke={2.2} /></button>
</div>
