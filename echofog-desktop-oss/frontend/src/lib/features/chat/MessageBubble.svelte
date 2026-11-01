<script>
  import { react, preview } from '../../stores/chats.js';
  import { savedIds } from '../../stores/saved.js';
  import { me } from '../../stores/session.js';
  import { time } from '../../format.js';
  import { linkify, mark, linkHost } from '../../linkify.js';
  import { t, lang } from '../../i18n/index.js';
  import Icon from '../../components/ui/Icon.svelte';
  import FileCard from '../../components/FileCard.svelte';
  import InviteCard from './InviteCard.svelte';
  import { findGroupLink } from '../../links.js';
  import { useChat, stateOf } from './context.js';

  /** One message of the main history: its quote, text or file, marks, reactions and comments. */
  export let m;
  export let conv;
  export let first = false;
  export let last = false;
  export let q = ''; // the search, to mark in the text
  export let hit = false;
  export let current = false;
  export let flash = false;

  const chat = useChat();
  const { nameOf, quoteOf, authorName } = chat;
  $: isSaved = $savedIds.has(conv.id + '/' + m.id);
  $: quoted = m.replyTo && !m.deleted ? $quoteOf(m.replyTo) : null;
  $: groupLink = !m.deleted && !m.file ? findGroupLink(m.text) : '';
</script>

<!-- svelte-ignore a11y-no-static-element-interactions -->
<div class="msg" data-mid={m.id} class:mine={m.mine} class:pending={m.pending} class:failed={m.failed} class:first class:last class:gone={m.deleted}
  class:hit class:current class:flash
  on:contextmenu={(e) => chat.openMenu(e, m)} on:dblclick={() => m.can.reply && !m.deleted && chat.startReply(m)}>
  {#if conv.kind === 'group' && !m.mine && first}<div class="sender small">{$nameOf(m.sender)}</div>{/if}
  <button class="more" title={$t('chats.msgActions')} aria-label={$t('chats.msgActions')} on:click={(e) => chat.openMenu(e, m)}><Icon name="chevronDown" size={15} /></button>
  {#if m.replyTo && !m.deleted}
    <button class="quote" on:click|stopPropagation={() => chat.goTo(m.replyTo)} disabled={!quoted}>
      {#if quoted}
        <b>{$authorName(quoted)}</b>
        <span>{preview(quoted)}</span>
      {:else}
        <span class="faint">{$t('chats.quoteUnavailable')}</span>
      {/if}
    </button>
  {/if}
  {#if m.deleted}
    <span class="text gone-text"><Icon name="trash" size={13} /> {$t('chats.deletedMsg')}</span>
  {:else if m.withheld}
    <span class="text gone-text">{$t('chats.withheld')}</span>
  {:else if m.file}
    <FileCard file={m.file} mine={m.mine} />
  {:else}
    <span class="text">{#each mark(linkify(m.text), q) as p}{#if p.href}<a href={p.href} title={linkHost(p.href)} rel="noreferrer noopener" on:click={(e) => chat.openLink(e, p.href)}>{#if p.mark}<mark>{p.text}</mark>{:else}{p.text}{/if}</a>{:else if p.mark}<mark>{p.text}</mark>{:else}{p.text}{/if}{/each}</span>
    {#if groupLink}<InviteCard link={groupLink} mine={m.mine} />{/if}
  {/if}
  <span class="meta">
    {#if m.pinned}<span class="saved-mark" title={$t('chats.pinned')}><Icon name="pin" size={11} stroke={2.4} /></span>{/if}
    {#if isSaved}<span class="saved-mark" title={$t('saved.saved')}><Icon name="bookmark" size={11} stroke={2.4} /></span>{/if}
    {#if m.edited && !m.deleted}<span class="edited" title={chat.fullTime(m.editedAt)}>{$t('chats.edited')}</span>{/if}
    {time(m.ts, $lang)}
    {#if m.mine && conv.kind === 'direct' && !m.deleted}
      {#if m.failed}<span class="st err" title={$t('chats.st.notSent')}>!</span>
      {:else if m.pending}<span class="st" class:relayed={m.state === 'relayed'} title={$t('chats.st.' + stateOf(m))}><Icon name={m.state === 'sent' || m.state === 'relayed' ? 'check' : 'clock'} size={13} stroke={2.4} /></span>
      {:else}<span class="st" title={$t('chats.st.delivered')}><Icon name="checks" size={14} stroke={2.2} /></span>{/if}
    {/if}
  </span>
  {#if !m.deleted && m.reactions.length}
    <div class="reactions">
      {#each m.reactions as r (r.emoji)}
        <button class="rx" class:on={r.mine} disabled={!m.can.react} title={r.by.map((p) => (p === $me.id ? $t('chats.You') : $nameOf(p))).join(', ')}
          on:click|stopPropagation={() => react(m.id, r.emoji, !r.mine)}>{r.emoji}<span>{r.count}</span></button>
      {/each}
    </div>
  {/if}
  {#if !m.deleted && m.replies > 0}
    <button class="replies" on:click|stopPropagation={() => chat.showThread(m)}><Icon name="message" size={13} /> {$t('chats.comments', { n: m.replies })}</button>
  {/if}
</div>
{#if m.failed}<div class="fail-note small">{$t('chats.note.notSent')}</div>{:else if m.pending && last}<div class="fail-note small pend">{$t('chats.note.' + stateOf(m))}</div>{/if}
