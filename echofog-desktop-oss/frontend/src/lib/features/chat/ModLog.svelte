<script>
  import { App, errText } from '../../api.js';
  import { membersVersion, openChannel } from '../../stores/chats.js';
  import { jumpTo } from '../../stores/saved.js';
  import { me } from '../../stores/session.js';
  import { t, intlLocale } from '../../i18n/index.js';
  import Icon from '../../components/ui/Icon.svelte';
  import { useChat } from './context.js';

  /** A group's moderation log (GroupModerationLog), read from its log: who let whom in, removed,
   *  banned, gave a role or a timeout, made or ended roles and invitations, deleted messages.
   *  Folded until opened; it is the same for every member. */
  export let conv;
  export let roles = [];

  const { nameOf } = useChat();
  let open = false;
  let list = null;
  let error = '';
  let shown = 30;

  $: if (open) { void $membersVersion; load(conv.groupId); }
  async function load(id) {
    try {
      list = (await App.GroupModerationLog(id)) || [];
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }
  const who = (id) => (!id ? '' : id === $me.id ? $t('chats.You') : $nameOf(id));
  const roleName = (id) => {
    const r = roles.find((x) => x.id === id);
    return r?.title || (r && !r.builtin ? id : id ? $t('chats.role.' + id) : '');
  };
  const when = (ms) => new Date(ms).toLocaleString(intlLocale(), { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
  const line = (e) => $t('chats.mod.' + e.action, {
    actor: who(e.actor), target: who(e.target), role: e.detail || roleName(e.role), label: e.detail || '',
    until: e.until ? when(e.until * 1000) : '',
  });
  // the message stays in the log as a deleted one: the chat shows where it was
  async function goTo(e) {
    await openChannel(e.channel || '');
    jumpTo.set({ chatId: conv.id, msgId: e.message });
  }
</script>

<div class="block">
  <button class="mod-head" aria-expanded={open} on:click={() => (open = !open)}>
    <Icon name={open ? 'chevronDown' : 'chevronRight'} size={13} /><span class="eyebrow grow">{$t('chats.mod.title')}</span>
  </button>
  {#if open}
    {#if list && list.length}
      <div class="mod-list">
        {#each list.slice(0, shown) as e, i (i)}
          <div class="mod-row">
            <span class="small faint ts">{when(e.ts)}</span>
            <span class="small grow">{line(e)}</span>
            {#if e.message}<button class="icon tiny" title={$t('chats.mod.show')} aria-label={$t('chats.mod.show')} on:click={() => goTo(e)}><Icon name="arrowDown" size={12} /></button>{/if}
          </div>
        {/each}
      </div>
      {#if list.length > shown}<button class="ghost small" on:click={() => (shown += 50)}>{$t('chats.mod.more')}</button>{/if}
    {:else if list}
      <p class="small faint">{$t('chats.mod.empty')}</p>
    {/if}
    {#if error}<p class="error small">{error}</p>{/if}
  {/if}
</div>

<style>
  .mod-head { display: flex; align-items: center; gap: 4px; width: 100%; padding: 0; background: none; border: 0; color: inherit; cursor: pointer; text-align: left; }
  .mod-list { display: flex; flex-direction: column; gap: 4px; margin-top: 8px; }
  .mod-row { display: flex; align-items: baseline; gap: 8px; }
  .ts { flex: none; width: 74px; }
</style>
