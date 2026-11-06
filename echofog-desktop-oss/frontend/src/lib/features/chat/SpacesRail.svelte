<script>
  import { chats, activeId, openChat, activeGroupCalls } from '../../stores/chats.js';
  import { view, chatsAction } from '../../stores/nav.js';
  import { openLinkDialog } from '../../links.js';
  import { t } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import Tooltip from '../../components/ui/Tooltip.svelte';
  import Popover from '../../components/ui/Popover.svelte';

  /** The spaces as avatars in a column, the way a Discord server list is: "Personal" on top (the
   *  list of one-to-one chats), then every space (a circle with the unread count, a dot while a
   *  call is going on, drag to change the order), then "+" (create, join by invite, open a link).
   *  wide: the names are written beside the avatars (the wide rail). */
  export let wide = false;

  const ORDER = 'echofog.spaceOrder';
  let order = [];
  try { order = JSON.parse(localStorage.getItem(ORDER) || '[]'); } catch (_) {}

  $: spaces = [...$chats.filter((c) => c.kind === 'group')].sort((a, b) => {
    const ia = order.indexOf(a.id);
    const ib = order.indexOf(b.id);
    if (ia >= 0 && ib >= 0) return ia - ib;
    if (ia >= 0) return -1;
    if (ib >= 0) return 1;
    return (b.ts || 0) - (a.ts || 0);
  });
  $: personalUnread = $chats.filter((c) => c.kind !== 'group').reduce((n, c) => n + (c.unread || 0), 0);
  $: active = $chats.find((c) => c.id === $activeId);
  $: inSpace = !!active && active.kind === 'group';
  $: inPersonal = $view === 'chats' && !inSpace;

  function goPersonal() {
    view.set('chats');
    // a one-to-one chat that is open stays; from a space the list of people shows
    if (inSpace) activeId.set('');
  }
  function goSpace(c) {
    view.set('chats');
    openChat(c.id);
  }

  // ---- order by dragging ----
  let dragged = '';
  let over = '';
  function dragStart(e, c) {
    dragged = c.id;
    e.dataTransfer.effectAllowed = 'move';
    try { e.dataTransfer.setData('text/plain', c.id); } catch (_) {}
  }
  function dragOver(e, c) {
    if (!dragged || dragged === c.id) return;
    e.preventDefault();
    over = c.id;
  }
  function drop(e, c) {
    e.preventDefault();
    const from = dragged;
    dragged = over = '';
    if (!from || from === c.id) return;
    const ids = spaces.map((s) => s.id).filter((id) => id !== from);
    ids.splice(ids.indexOf(c.id), 0, from);
    order = ids;
    try { localStorage.setItem(ORDER, JSON.stringify(order)); } catch (_) {}
  }

  // ---- "+" ----
  let plusBtn;
  let menu = false;
  function start(p) {
    menu = false;
    view.set('chats');
    chatsAction.set(p);
  }
</script>

<div class="spaces" class:wide role="navigation" aria-label={$t('spaces.title')}>
  <Tooltip text={wide ? '' : $t('spaces.personal')} placement="right">
    <button class="sp personal" class:on={inPersonal} aria-current={inPersonal ? 'page' : undefined} aria-label={$t('spaces.personal')} on:click={goPersonal}>
      <span class="av home"><Icon name="user" size={18} />{#if personalUnread}<span class="cnt">{personalUnread > 99 ? '99+' : personalUnread}</span>{/if}</span>
      {#if wide}<span class="nm">{$t('spaces.personal')}</span>{/if}
    </button>
  </Tooltip>
  {#if spaces.length}<span class="sep" aria-hidden="true"></span>{/if}
  {#each spaces as c (c.id)}
    {@const here = inSpace && c.id === $activeId && $view === 'chats'}
    <Tooltip text={wide ? '' : c.name} placement="right">
      <!-- svelte-ignore a11y-no-static-element-interactions -->
      <button class="sp" class:on={here} class:over={over === c.id} draggable="true" aria-current={here ? 'page' : undefined} aria-label={c.name}
        on:click={() => goSpace(c)} on:dragstart={(e) => dragStart(e, c)} on:dragover={(e) => dragOver(e, c)} on:dragleave={() => (over = '')}
        on:drop={(e) => drop(e, c)} on:dragend={() => (dragged = over = '')}>
        <span class="av">
          <Avatar id={c.id} name={c.name} src={c.avatar} size={36} />
          {#if c.unread}<span class="cnt" class:ment={c.mentions}>{c.unread > 99 ? '99+' : c.unread}</span>{/if}
          {#if $activeGroupCalls[c.id]}<span class="call" title={$t('chats.callActive')}></span>{/if}
        </span>
        {#if wide}<span class="nm" class:unread={c.unread}>{c.name}</span>{/if}
      </button>
    </Tooltip>
  {/each}
  <Tooltip text={wide ? '' : $t('spaces.add')} placement="right">
    <button class="sp add" bind:this={plusBtn} class:on={menu} aria-haspopup="menu" aria-expanded={menu} aria-label={$t('spaces.add')} on:click={() => (menu = !menu)}>
      <span class="av plus"><Icon name="plus" size={18} /></span>
      {#if wide}<span class="nm">{$t('spaces.add')}</span>{/if}
    </button>
  </Tooltip>
</div>

{#if menu}
  <Popover anchor={plusBtn} width={250} label={$t('spaces.add')} on:close={() => (menu = false)}>
    <div class="menu">
      <button class="mi" on:click={() => start('group')}><Icon name="users" size={17} /><span>{$t('chats.newSpace')}</span></button>
      <button class="mi" on:click={() => start('join')}><Icon name="link" size={17} /><span>{$t('chats.joinInvite')}</span></button>
      <button class="mi" on:click={() => { menu = false; openLinkDialog(''); }}><Icon name="link" size={17} /><span>{$t('links.title')}</span><kbd class="small faint">Ctrl+L</kbd></button>
    </div>
  </Popover>
{/if}

<style>
  .spaces { display: flex; flex-direction: column; align-items: center; gap: 6px; padding: 2px 0; min-height: 0; }
  .spaces.wide { align-items: stretch; }
  .sp {
    position: relative; display: flex; align-items: center; gap: 12px; padding: 0; background: transparent; border: none;
    border-radius: var(--r-md); color: var(--muted); font-weight: var(--fw-medium); font-size: var(--fs-md); text-align: left;
  }
  .spaces:not(.wide) .sp { width: 40px; height: 40px; justify-content: center; }
  .spaces.wide .sp { height: 40px; padding: 0 2px; }
  .sp:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .sp.on, .sp.on:hover:not(:disabled) { background: var(--surface-3); color: var(--text); }
  .sp.on::before { content: ''; position: absolute; left: -8px; top: 10px; bottom: 10px; width: 3px; border-radius: 0 3px 3px 0; background: var(--accent); }
  .sp.over { outline: 2px dashed var(--accent); outline-offset: 1px; }
  .av { position: relative; flex: none; width: 36px; height: 36px; display: grid; place-items: center; }
  .av.home, .av.plus { border-radius: 50%; background: var(--surface-2); color: var(--accent-fg); }
  .sp.on .av.home { background: var(--accent-soft); }
  .sp:hover .av.plus { background: var(--accent); color: var(--accent-ink); }
  .nm { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .nm.unread { color: var(--text); font-weight: var(--fw-semibold); }
  .cnt {
    position: absolute; top: -4px; right: -6px; min-width: 16px; height: 16px; padding: 0 4px; border-radius: var(--r-pill);
    background: var(--accent); color: var(--accent-ink); font-size: 10px; font-weight: var(--fw-bold); display: grid; place-items: center;
    box-shadow: 0 0 0 2px var(--surface-1);
  }
  .cnt.ment { background: var(--danger); color: var(--danger-ink); }
  .call { position: absolute; bottom: -1px; right: -3px; width: 10px; height: 10px; border-radius: 50%; background: var(--ok); box-shadow: 0 0 0 2px var(--surface-1); animation: sp-live 1.8s infinite; }
  @keyframes sp-live { 50% { opacity: 0.45; } }
  .sep { width: 24px; height: 1px; background: var(--border); margin: 2px 0; }
  .menu { display: flex; flex-direction: column; padding: 4px; gap: 1px; }
  .mi { display: flex; align-items: center; gap: 10px; width: 100%; padding: 7px 10px; border: none; border-radius: var(--r-md); background: transparent; text-align: left; font-weight: var(--fw-medium); font-size: var(--fs-md); }
  .mi:hover:not(:disabled) { background: var(--hover); border: none; }
</style>
