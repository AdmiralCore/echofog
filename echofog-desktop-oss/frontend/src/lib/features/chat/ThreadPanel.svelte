<script>
  import { onDestroy } from 'svelte';
  import { fly } from 'svelte/transition';
  import { thread, closeThread, send, editMessage, react, preview, fetchQuote } from '../../stores/chats.js';
  import { draftOf, keepDraft } from '../../stores/drafts.js';
  import { me } from '../../stores/session.js';
  import { time } from '../../format.js';
  import { linkify, linkHost } from '../../linkify.js';
  import { t, lang } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import Composer from './Composer.svelte';
  import { useChat, stateOf } from './context.js';

  /** The comments under a message, and where to write one. */
  export let conv;

  const chat = useChat();
  const { quoteOf, authorName } = chat;
  let composer;
  let text = '';
  let editing = null;
  let sending = false;

  // the panel slides out after the thread closed: it keeps showing the last comments meanwhile
  let comments = [];
  $: if ($thread) comments = $thread.messages;
  $: root = $thread ? $quoteOf($thread.root) : root;
  $: if ($thread && !root) fetchQuote($thread.root);

  // the draft of each thread is kept apart, and the field takes the cursor when a thread opens
  let key = '';
  $: switchTo($thread ? conv.id + '#' + $thread.root : '');
  function switchTo(k) {
    if (k === key) return;
    keepDraft(key, { text, editing });
    key = k;
    const d = draftOf(k);
    text = d.text;
    editing = d.editing;
  }
  // the field takes the cursor once it is there, and again for every other thread opened
  let focusedKey = '';
  $: if (composer && key && key !== focusedKey) {
    focusedKey = key;
    composer.focus();
  }
  onDestroy(() => keepDraft(key, { text, editing }));

  /** Starts changing a comment of mine. */
  export function edit(m) {
    editing = m;
    text = m.text;
    composer?.focus();
  }

  async function submit() {
    if (!text.trim() || sending || !$thread) return;
    sending = true;
    const ok = editing
      ? editing.text === text.trim() || (await editMessage(editing.id, text))
      : await send(text, { threadRoot: $thread.root });
    if (ok) { text = ''; editing = null; }
    sending = false;
  }
</script>

<aside class="info thread" transition:fly={{ x: 40, duration: 280, opacity: 0 }}>
  <div class="info-head">
    <span class="eyebrow grow">{$t('chats.threadTitle')}</span>
    <button class="icon" aria-label={$t('common.close')} on:click={closeThread}><Icon name="close" size={17} /></button>
  </div>
  {#if root}
    <button class="root" on:click={() => chat.goTo(root.id)}>
      <b class="small">{$authorName(root)}</b>
      <span>{root.deleted ? $t('chats.deletedMsg') : preview(root)}</span>
      <span class="faint small">{time(root.ts, $lang)}</span>
    </button>
  {/if}
  <div class="comments">
    {#each comments as c (c.id)}
      <!-- svelte-ignore a11y-no-static-element-interactions -->
      <div class="comment" class:mine={c.mine} data-cid={c.id} on:contextmenu={(e) => chat.openMenu(e, c)}>
        <Avatar id={c.mine ? $me.id : c.sender} name={$authorName(c)} size={28} />
        <div class="grow c-body">
          <div class="c-head small"><b>{$authorName(c)}</b><span class="faint">{time(c.ts, $lang)}{#if c.edited && !c.deleted} · {$t('chats.edited')}{/if}</span>
            <button class="more" title={$t('chats.msgActions')} aria-label={$t('chats.msgActions')} on:click={(e) => chat.openMenu(e, c)}><Icon name="chevronDown" size={14} /></button>
          </div>
          {#if c.deleted}
            <span class="gone-text small">{$t('chats.deletedMsg')}</span>
          {:else}
            <span class="text">{#each linkify(c.text) as p}{#if p.href}<a href={p.href} title={linkHost(p.href)} rel="noreferrer noopener" on:click={(e) => chat.openLink(e, p.href)}>{p.text}</a>{:else}{p.text}{/if}{/each}</span>
          {/if}
          {#if !c.deleted && c.reactions.length}
            <div class="reactions">
              {#each c.reactions as r (r.emoji)}
                <button class="rx" class:on={r.mine} disabled={!c.can.react} on:click={() => react(c.id, r.emoji, !r.mine)}>{r.emoji}<span>{r.count}</span></button>
              {/each}
            </div>
          {/if}
          {#if c.mine && conv.kind === 'direct' && c.pending}<span class="small faint">{$t('chats.note.' + stateOf(c))}</span>{/if}
        </div>
      </div>
    {:else}
      <p class="small faint">{$t('chats.noComments')}</p>
    {/each}
  </div>
  {#if conv.can.send}
    <Composer bind:this={composer} bind:text {editing} thread busy={sending} placeholder={$t('chats.writeComment')}
      on:submit={submit} on:cancel={() => { editing = null; text = ''; }} on:escape={closeThread} on:goto={(e) => chat.goTo(e.detail)} />
  {/if}
</aside>
