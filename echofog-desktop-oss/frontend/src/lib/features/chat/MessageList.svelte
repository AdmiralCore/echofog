<script>
  import { afterUpdate, onDestroy, onMount, tick } from 'svelte';
  import { messages, hasOlder, loadOlder, markRead } from '../../stores/chats.js';
  import { jumpTo } from '../../stores/saved.js';
  import { time } from '../../format.js';
  import { t, lang, intlLocale } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import MessageBubble from './MessageBubble.svelte';
  import { sameGroup, dayKey } from './context.js';

  /** The main history: days, message groups, scrolling, older pages, and the read point. */
  export let conv;
  export let q = ''; // the search
  export let matches = []; // ids that match it
  export let currentMatch = '';

  let scroller;

  // ---- scrolling ---------------------------------------------------------------------------------
  // The end of the chat stays in view while I am there; scrolling to the top loads older messages
  // without moving what is on screen.
  let newestId = '';
  let atBottom = true;
  let prepending = null; // { height, top, firstId } before an older page is put in front
  let shownId = '';
  $: if (conv.id !== shownId) {
    shownId = conv.id;
    newestId = '';
    lastRead = '';
    atBottom = true;
  }

  afterUpdate(() => {
    if (pendingJump && $messages.some((m) => m.id === pendingJump)) {
      const id = pendingJump;
      pendingJump = '';
      newestId = $messages[$messages.length - 1].id;
      reveal(id);
      return;
    }
    if (!scroller) return;
    if (prepending && $messages.length && $messages[0].id !== prepending.firstId) {
      scroller.scrollTop = scroller.scrollHeight - prepending.height + prepending.top;
      prepending = null;
      return;
    }
    const last = $messages[$messages.length - 1];
    const id = last ? last.id : '';
    if (id !== newestId) {
      // opening a chat jumps straight to the end; a new message glides into view
      const first = !newestId;
      newestId = id;
      if (first || atBottom || (last && last.mine)) {
        scroller.scrollTo({ top: scroller.scrollHeight, behavior: first ? 'auto' : 'smooth' });
        atBottom = true;
        readSoon();
      }
    }
    // a first page that does not fill the window: fetch more until it does
    if (!prepending && $hasOlder && scroller.scrollHeight <= scroller.clientHeight + 40) older();
  });

  function onScroll() {
    atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80;
    if (scroller.scrollTop < 160 && $hasOlder && !prepending) older();
    if (atBottom) readSoon();
  }

  async function older() {
    prepending = { height: scroller.scrollHeight, top: scroller.scrollTop, firstId: $messages[0]?.id || '' };
    if (!(await loadOlder())) prepending = null;
  }

  // ---- read -------------------------------------------------------------------------------------
  // The chat counts as read up to its newest message once that is on screen in a window in front.
  let lastRead = '';
  let readTimer;
  function readSoon() {
    clearTimeout(readTimer);
    readTimer = setTimeout(() => {
      const last = $messages[$messages.length - 1];
      if (!last || !atBottom || !document.hasFocus()) return;
      if (last.id === lastRead && !conv.unread) return;
      lastRead = last.id;
      markRead(last.id);
    }, 1000);
  }

  /** Scrolls a message into view; flash marks it for a moment. */
  let flashId = '';
  export async function reveal(id, flash = true) {
    await tick();
    const el = scroller?.querySelector(`[data-mid="${CSS.escape(id)}"]`);
    if (!el) return;
    el.scrollIntoView({ block: 'center', behavior: 'smooth' });
    if (flash) {
      flashId = id;
      setTimeout(() => { if (flashId === id) flashId = ''; }, 1600);
    }
  }

  /** Scrolls to a message, loading older pages until it is there (a pin or a quote far back). */
  export async function goTo(id) {
    for (let i = 0; i < 40 && !$messages.some((m) => m.id === id); i++) {
      if (!$hasOlder) break;
      prepending = { height: scroller.scrollHeight, top: scroller.scrollTop, firstId: $messages[0]?.id || '' };
      if (!(await loadOlder())) { prepending = null; break; }
      await tick();
    }
    reveal(id);
  }

  // a saved message opened from the saved list: scroll to it once it is loaded
  let pendingJump = '';
  const unJump = jumpTo.subscribe((j) => {
    if (!j || j.chatId !== conv.id) return;
    jumpTo.set(null);
    if ($messages.some((m) => m.id === j.msgId)) reveal(j.msgId);
    else pendingJump = j.msgId;
  });

  // the area shrinks when a bar appears above it (pins) or the composer grows: stay at the end
  const ro = new ResizeObserver(() => {
    if (atBottom && scroller) scroller.scrollTop = scroller.scrollHeight;
  });
  onMount(() => { if (scroller) ro.observe(scroller); });
  onDestroy(() => {
    ro.disconnect();
    clearTimeout(readTimer);
    unJump();
  });

  function dayLabel(ts) {
    const d = new Date(ts || 0);
    const today = new Date();
    if (d.toDateString() === today.toDateString()) return $t('chats.today');
    if (d.toDateString() === new Date(Date.now() - 864e5).toDateString()) return $t('chats.yesterday');
    return d.toLocaleDateString(intlLocale(), { day: 'numeric', month: 'long', ...(d.getFullYear() === today.getFullYear() ? {} : { year: 'numeric' }) });
  }
</script>

<div class="messages" bind:this={scroller} on:scroll={onScroll}>
  {#if $hasOlder}<div class="older small faint">{$t('chats.loadingOlder')}</div>{/if}
  {#each $messages as m, i (m.id)}
    {#if i === 0 || dayKey($messages[i - 1].ts) !== dayKey(m.ts)}
      <div class="day"><span>{dayLabel(m.ts, $lang)}</span></div>
    {/if}
    {#if m.system}
      <div class="system small" data-mid={m.id}>{m.text} · {time(m.ts, $lang)}</div>
    {:else}
      <MessageBubble {m} {conv} {q}
        first={!sameGroup($messages[i - 1], m) || dayKey($messages[i - 1].ts) !== dayKey(m.ts)}
        last={!sameGroup(m, $messages[i + 1]) || dayKey($messages[i + 1].ts) !== dayKey(m.ts)}
        hit={!!q && matches.includes(m.id)} current={m.id === currentMatch} flash={m.id === flashId} />
    {/if}
  {:else}
    <div class="empty">
      <Avatar id={conv.peerId || conv.id} name={conv.name} size={64} />
      <p class="small muted">{$t('chats.sayHi', { name: conv.name })}</p>
    </div>
  {/each}
</div>
