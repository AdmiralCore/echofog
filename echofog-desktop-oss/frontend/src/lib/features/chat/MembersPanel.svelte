<script>
  import { createEventDispatcher } from 'svelte';
  import { App } from '../../api.js';
  import { membersVersion } from '../../stores/chats.js';
  import { t } from '../../i18n/index.js';
  import { hexColor } from '../../perms.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import Skeleton from '../../components/ui/Skeleton.svelte';
  import { useChat } from './context.js';

  /** A space's members by role, always at hand at the right of the chat (the full list with what a
   *  manager may do to each is in the space settings, one button away). */
  export let conv;
  /** a narrow chat: the panel floats over it instead of taking room */
  export let float = false;

  const dispatch = createEventDispatcher();
  const { nameOf } = useChat();

  let members = null;
  let roles = [];
  $: void $membersVersion, load(conv.groupId);
  async function load(id) {
    try {
      const [m, r] = await Promise.all([App.ListGroupMembers(id), App.GroupRoles(id).catch(() => [])]);
      members = m || [];
      roles = r || [];
    } catch (_) {
      members = members || [];
    }
  }

  const roleOf = (id) => roles.find((r) => r.id === id);
  const roleLabel = (id, title) => title || roleOf(id)?.title || (roleOf(id) && !roleOf(id).builtin ? id : $t('chats.role.' + id));
  $: order = ['owner', ...roles.map((r) => r.id)];
  $: groups = (() => {
    const by = new Map();
    for (const m of members || []) {
      if (!by.has(m.role)) by.set(m.role, []);
      by.get(m.role).push(m);
    }
    const rank = (id) => { const i = order.indexOf(id); return i < 0 ? 999 : i; };
    return [...by.entries()].sort((a, b) => rank(a[0]) - rank(b[0])).map(([id, list]) => ({
      id, title: list[0].roleTitle, color: hexColor(roleOf(id)?.color || 0),
      list: [...list].sort((a, b) => Number(!!b.online) - Number(!!a.online)),
    }));
  })();
  const nameOfMember = (m) => (m.isMe ? $t('common.you') : $nameOf(m.peerId));
</script>

<aside class="mpanel" class:float aria-label={$t('chats.membersPanel')}>
  <div class="mhead">
    <div class="grow">
      <h3>{$t('chats.membersPanel')}</h3>
      <p class="small faint">{$t('chats.members', { n: members ? members.length : conv.members })}</p>
    </div>
    <button class="icon tiny" title={$t('chats.spaceSettings')} aria-label={$t('chats.spaceSettings')} on:click={() => dispatch('settings')}><Icon name="settings" size={15} /></button>
    <button class="icon tiny" title={$t('chats.closeInfo')} aria-label={$t('chats.closeInfo')} on:click={() => dispatch('close')}><Icon name="close" size={15} /></button>
  </div>
  <div class="mlist">
    {#if members === null}
      {#each [0, 1, 2, 3] as i (i)}<div class="mrow-sk"><Skeleton circle height={28} /><Skeleton width="60%" height={12} /></div>{/each}
    {/if}
    {#each groups as g (g.id)}
      <div class="rhead">{#if g.color}<span class="rd" style:background={g.color}></span>{/if}{$t('chats.roleCount', { role: roleLabel(g.id, g.title), n: g.list.length })}</div>
      {#each g.list as m (m.peerId)}
        <div class="mem" class:off={!m.online} title={m.online ? $t('common.online') : $t('common.offline')}>
          <span class="av"><Avatar id={m.peerId} name={nameOfMember(m)} size={28} /><span class="presence" class:on={m.online}></span></span>
          <span class="nm" style:color={m.online && g.color ? g.color : null}>{nameOfMember(m)}</span>
          {#if m.timeout}<span class="to" title={$t('chats.to.until', { when: new Date(m.timeout * 1000).toLocaleString() })}><Icon name="clock" size={12} /></span>{/if}
        </div>
      {/each}
    {/each}
  </div>
</aside>

<style>
  .mpanel { width: 224px; flex: none; display: flex; flex-direction: column; min-height: 0; border-left: 1px solid var(--border); background: var(--surface-1); }
  .mpanel.float { position: absolute; right: 0; top: 0; bottom: 0; z-index: 10; box-shadow: var(--shadow-lg); max-width: 100%; }
  .mhead { display: flex; align-items: center; gap: 2px; padding: 8px 8px 8px 14px; min-height: 56px; border-bottom: 1px solid var(--border); }
  .mhead h3 { font-size: var(--fs-md); line-height: var(--lh-md); }
  .icon.tiny { width: var(--ctl-sm); height: var(--ctl-sm); border-radius: var(--r-md); }
  .mlist { flex: 1; overflow-y: auto; padding: 4px 8px 12px; }
  .rhead { display: flex; align-items: center; gap: 6px; padding: 12px 6px 4px; font-size: var(--fs-2xs); font-weight: var(--fw-bold); letter-spacing: 0.04em; text-transform: uppercase; color: var(--faint); }
  .rd { width: 8px; height: 8px; border-radius: 50%; flex: none; }
  .mem { display: flex; align-items: center; gap: 10px; padding: 4px 6px; border-radius: var(--r-md); }
  .mem:hover { background: var(--hover); }
  .mem.off { opacity: 0.55; }
  .av { position: relative; flex: none; display: inline-flex; }
  .presence { position: absolute; right: -2px; bottom: -2px; width: 10px; height: 10px; border-radius: 50%; background: var(--faint); border: 2px solid var(--surface-1); }
  .presence.on { background: var(--ok); }
  .nm { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: var(--fs-md); font-weight: var(--fw-medium); }
  .to { display: inline-flex; color: var(--warn); }
  .mrow-sk { display: flex; align-items: center; gap: 10px; padding: 6px; }
</style>
