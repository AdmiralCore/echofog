<script>
  import { createEventDispatcher, onMount, onDestroy } from 'svelte';
  import { fly } from 'svelte/transition';
  import { threads, threadsShown, loadThreads, openThread, preview, activeChannel } from '../../stores/chats.js';
  import { when } from '../../format.js';
  import { t, lang } from '../../i18n/index.js';
  import Icon from '../../components/ui/Icon.svelte';
  import { useChat } from './context.js';

  /** A channel's threads, the most recently active first: their names (or first words) and how many
   *  comments each has. */
  const dispatch = createEventDispatcher();
  const { authorName } = useChat();
  onMount(() => { threadsShown.set(true); loadThreads(); });
  onDestroy(() => threadsShown.set(false));
  $: $activeChannel, loadThreads();

  async function open(m) {
    dispatch('close');
    await openThread(m.id);
  }
</script>

<aside class="info thread" transition:fly={{ x: 40, duration: 280, opacity: 0 }}>
  <div class="info-head">
    <span class="eyebrow grow">{$t('chats.threads')}</span>
    <button class="icon" aria-label={$t('common.close')} on:click={() => dispatch('close')}><Icon name="close" size={17} /></button>
  </div>
  <div class="threads">
    {#each $threads as m (m.id)}
      <button class="th-row" on:click={() => open(m)}>
        <span class="th-name">{m.threadTitle || preview(m)}</span>
        <span class="small faint">{$authorName(m)} · {$t('chats.comments', { n: m.replies })}{#if m.lastReplyAt} · {when(m.lastReplyAt, $lang)}{/if}</span>
      </button>
    {:else}
      <p class="small faint">{$t('chats.noThreads')}</p>
    {/each}
  </div>
</aside>

<style>
  .threads { display: flex; flex-direction: column; gap: 2px; }
  .th-row {
    display: flex; flex-direction: column; align-items: flex-start; gap: 2px; padding: 9px 10px; border: none; border-radius: 10px;
    background: transparent; text-align: left; font-weight: 400;
  }
  .th-row:hover:not(:disabled) { background: var(--hover); border: none; }
  .th-name { font-weight: 600; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
