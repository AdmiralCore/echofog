<script>
  /** Messages the user bookmarked, from every chat. Each opens the chat at the message. */
  import { createEventDispatcher } from 'svelte';
  import { fly } from 'svelte/transition';
  import { saved, savedError, unsave, saveAsFile, jumpTo } from '../stores/saved.js';
  import { chats, openChat, openChannel, openThread } from '../stores/chats.js';
  import { copy, openURL } from '../api.js';
  import { bytes, when, time } from '../format.js';
  import { linkify, mark, linkHost } from '../linkify.js';
  import { t, lang } from '../i18n/index.js';
  import Icon from '../components/ui/Icon.svelte';
  import Avatar from '../components/ui/Avatar.svelte';

  export let back = false;
  export let indent = false;
  const dispatch = createEventDispatcher();

  let query = '';
  let copied = '';
  $: q = query.trim().toLowerCase();
  $: list = $saved.filter((m) => !q || (m.text + ' ' + (m.fileName || '') + ' ' + m.chatName).toLowerCase().includes(q));
  $: known = new Set($chats.map((c) => c.id));

  // to where the message is: the chat, then its channel, then (a comment) its thread, whose root
  // is the message the chat scrolls to
  async function go(m) {
    if (!known.has(m.chatId)) return;
    await openChat(m.chatId);
    if (m.channel) await openChannel(m.channel);
    jumpTo.set({ chatId: m.chatId, msgId: m.thread || m.msgId });
    if (m.thread) await openThread(m.thread);
  }

  async function doCopy(m) {
    if (await copy(m.fileName || m.text)) {
      copied = m.id;
      setTimeout(() => (copied = ''), 1200);
    }
  }

  function openLink(e, href) {
    e.preventDefault();
    e.stopPropagation();
    openURL(href);
  }
</script>

<div class="saved">
  <header class:indent>
    {#if back}
      <button class="icon" title={$t('chats.backToChats')} aria-label={$t('chats.backToChats')} on:click={() => dispatch('back')}><Icon name="arrowLeft" size={19} /></button>
    {/if}
    <span class="badge-ic"><Icon name="bookmark" size={19} /></span>
    <div class="title grow">
      <h3>{$t('saved.title')}</h3>
      <p class="small faint">{$t('saved.count', { n: $saved.length })}</p>
    </div>
  </header>

  <div class="search">
    <Icon name="search" size={15} />
    <input placeholder={$t('saved.search')} bind:value={query} spellcheck="false" />
    {#if query}<button class="icon tiny" aria-label={$t('chats.clearSearch')} on:click={() => (query = '')}><Icon name="close" size={13} stroke={2.2} /></button>{/if}
  </div>

  {#if $savedError}<p class="error small bar">{$savedError}</p>{/if}

  <div class="list">
    {#each list as m (m.id)}
      <article class="item" in:fly={{ y: 8, duration: 220 }}>
        <div class="row head">
          <Avatar id={m.chatId.slice(2)} name={m.chatName} size={26} />
          <span class="chat grow">{m.chatName}{#if !m.mine && m.sender && m.chatId.startsWith('g:')}{' · '}{m.sender}{:else if m.mine}{' · '}{$t('chats.You')}{/if}</span>
          <span class="when small faint" title={time(m.ts, $lang)}>{when(m.ts, $lang)}</span>
        </div>
        {#if m.fileName}
          <div class="file"><Icon name="file" size={16} /><span class="grow fname">{m.fileName}</span><span class="small faint">{bytes(m.fileSize)}</span></div>
        {:else}
          <p class="text">{#each mark(linkify(m.text), q) as p}{#if p.href}<a href={p.href} title={linkHost(p.href)} rel="noreferrer noopener" on:click={(e) => openLink(e, p.href)}>{p.text}</a>{:else if p.mark}<mark>{p.text}</mark>{:else}{p.text}{/if}{/each}</p>
        {/if}
        <div class="row acts">
          <button class="ghost small with-icon" on:click={() => go(m)} disabled={!known.has(m.chatId)} title={known.has(m.chatId) ? '' : $t('saved.chatGone')}><Icon name="message" size={14} /> {$t('saved.goTo')}</button>
          <button class="ghost small with-icon" on:click={() => doCopy(m)}><Icon name={copied === m.id ? 'check' : 'copy'} size={14} /> {copied === m.id ? $t('common.copied') : $t('common.copy')}</button>
          {#if !m.fileName}<button class="ghost small with-icon" on:click={() => saveAsFile(m, m.chatName)}><Icon name="download" size={14} /> {$t('chats.saveAsFile')}</button>{/if}
          <span class="grow"></span>
          <button class="icon tiny" title={$t('saved.remove')} aria-label={$t('saved.remove')} on:click={() => unsave(m.id)}><Icon name="trash" size={14} /></button>
        </div>
      </article>
    {:else}
      <div class="empty">
        <span class="empty-ic"><Icon name="bookmark" size={22} /></span>
        <p class="small muted">{q ? $t('chats.nothingFound', { query }) : $t('saved.empty')}</p>
      </div>
    {/each}
  </div>
</div>

<style>
  .saved { flex: 1; min-height: 0; display: flex; flex-direction: column; }
  header {
    display: flex; align-items: center; gap: 12px; padding: 10px 16px; min-height: 64px;
    border-bottom: 1px solid var(--line-soft); background: var(--glass); backdrop-filter: blur(16px) saturate(1.3);
  }
  header.indent { padding-left: 58px; }
  .badge-ic { width: 40px; height: 40px; border-radius: 50%; display: grid; place-items: center; color: #fff; background: linear-gradient(135deg, var(--accent), var(--accent-2)); flex: none; }
  .title h3 { font-size: 16px; }
  .search {
    display: flex; align-items: center; gap: 8px; margin: 12px clamp(12px, 3vw, 28px) 4px; padding: 0 6px 0 12px; height: 38px;
    border-radius: 12px; background: var(--raised); color: var(--faint); border: 1px solid transparent;
  }
  .search:focus-within { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); background: var(--panel); color: var(--accent); }
  .search input { border: none; background: transparent; padding: 0; height: 100%; box-shadow: none; }
  .search input:focus { box-shadow: none; background: transparent; }
  .icon.tiny { width: 26px; height: 26px; border-radius: 8px; }
  .bar { padding: 6px 20px; }
  .list { flex: 1; overflow-y: auto; padding: 10px clamp(12px, 3vw, 28px) 18px; display: flex; flex-direction: column; gap: 10px; }
  .item { padding: 12px 14px 8px; border-radius: 16px; background: var(--panel); border: 1px solid var(--line-soft); box-shadow: var(--shadow-sm); display: flex; flex-direction: column; gap: 8px; max-width: 760px; }
  .head { gap: 8px; }
  .chat { font-weight: 620; font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .text { white-space: pre-wrap; overflow-wrap: anywhere; line-height: 1.45; margin: 0; }
  .text a { color: var(--accent); text-decoration: underline; text-underline-offset: 2px; }
  .text mark { background: color-mix(in srgb, #ffd54a 70%, transparent); color: inherit; border-radius: 3px; }
  .file { display: flex; align-items: center; gap: 8px; padding: 8px 10px; border-radius: 12px; background: var(--raised); color: var(--muted); }
  .fname { color: var(--text); font-weight: 550; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .acts { gap: 4px; flex-wrap: wrap; margin: 0 -6px; }
  .empty { margin: 40px auto; display: flex; flex-direction: column; align-items: center; gap: 12px; text-align: center; max-width: 320px; }
  .empty-ic { width: 48px; height: 48px; border-radius: 16px; display: grid; place-items: center; background: var(--accent-soft); color: var(--accent); }
</style>
