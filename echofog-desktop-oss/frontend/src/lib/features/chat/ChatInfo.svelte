<script>
  import { createEventDispatcher } from 'svelte';
  import { fly } from 'svelte/transition';
  import { App, errText } from '../../api.js';
  import { messages, hasOlder, membersVersion, activeId, loadChats, leaveGroup } from '../../stores/chats.js';
  import { transfers, openFile, showInFolder } from '../../stores/files.js';
  import { savedIds, SAVED_ID } from '../../stores/saved.js';
  import { bytes } from '../../format.js';
  import { t, intlLocale } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import GroupRoles from './GroupRoles.svelte';
  import GroupInvites from './GroupInvites.svelte';
  import GroupRequests from './GroupRequests.svelte';
  import ModLog from './ModLog.svelte';
  import { hexColor } from '../../perms.js';
  import { useChat } from './context.js';

  /** A group's side panel (a contact has ContactProfile): facts, files, the invitation and the
   *  members. What I may do to each member comes from the core (canKick, canBan, roles, canTransfer). */
  export let conv;

  const chat = useChat();
  const { nameOf } = chat;
  const dispatch = createEventDispatcher();

  let members = [];
  let error = '';
  let newMemberKey = '';
  let busy = ''; // the member being changed, or 'add'
  let roles = []; // the group's roles (GroupRoles)

  // a role's name: a group's own title, else the built-in one
  $: roleLabel = (id, title) => {
    const r = roles.find((x) => x.id === id);
    if (title || r?.title) return title || r.title;
    return r && !r.builtin ? id : $t('chats.role.' + id);
  };
  $: roleColor = (id) => hexColor(roles.find((x) => x.id === id)?.color || 0);

  $: group = conv.kind === 'group';
  $: if (group) { void $membersVersion; loadMembers(conv.groupId); }

  async function loadMembers(id) {
    try {
      members = (await App.ListGroupMembers(id)) || [];
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }


  async function memberAction(id, fn) {
    busy = id;
    error = '';
    try {
      await fn();
      await loadMembers(conv.groupId);
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }
  const nameOfMember = (m) => (m.isMe ? $t('common.you') : $nameOf(m.peerId));
  async function makeOwner(m) {
    if (!(await ask({ message: $t('chats.confirm.makeOwner', { name: $nameOf(m.peerId), group: conv.name }) }))) return;
    memberAction(m.peerId, async () => { await App.TransferGroupOwnership(conv.groupId, m.peerId); await loadChats(); });
  }
  const setRole = (m, role) => memberAction(m.peerId, () => App.SetGroupRole(conv.groupId, m.peerId, role));
  // a timeout: minutes, 0 lifts it
  const TIMEOUTS = [10, 60, 24 * 60, 7 * 24 * 60];
  let timing = ''; // the member whose timeout menu is open
  const timeoutLabel = (min) => (min < 60 ? $t('chats.to.min', { n: min }) : min < 24 * 60 ? $t('chats.to.hours', { n: min / 60 }) : $t('chats.to.days', { n: min / (24 * 60) }));
  function timeout(m, min) {
    timing = '';
    memberAction(m.peerId, () => App.GroupTimeout(conv.groupId, m.peerId, min));
  }
  const untilText = (s) => new Date(s * 1000).toLocaleString(intlLocale(), { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
  async function kick(m, ban) {
    if (!(await ask({ message: $t(ban ? 'chats.confirm.ban' : 'chats.confirm.remove', { name: $nameOf(m.peerId) }), confirm: $t('common.remove'), danger: true }))) return;
    memberAction(m.peerId, () => App.RemoveGroupMember(conv.groupId, m.peerId, ban));
  }
  async function addMember() {
    const key = newMemberKey.trim();
    if (!key) return;
    await memberAction('add', () => App.AddGroupMember(conv.groupId, key));
    if (!error) newMemberKey = '';
  }
  async function leave() {
    if (!(await ask({ message: $t('chats.confirm.leave', { name: conv.name }), danger: true }))) return;
    try { await leaveGroup(conv); } catch (err) { error = errText(err); }
  }
  async function deleteGroup() {
    if (!(await ask({ message: $t('chats.confirm.deleteGroup', { name: conv.name }), confirm: $t('common.delete'), danger: true }))) return;
    try { await App.DeleteGroup(conv.groupId); await loadChats(); activeId.set(''); } catch (err) { error = errText(err); }
  }


  $: firstTs = $hasOlder ? 0 : $messages.find((m) => !m.system)?.ts || 0;
  $: savedHere = [...$savedIds].filter((id) => id.startsWith(conv.id + '/')).length;
  $: msgCount = $messages.filter((m) => !m.system && !m.deleted).length;
  const sinceDate = (ts) => (ts ? new Date(ts).toLocaleDateString(intlLocale(), { day: '2-digit', month: '2-digit', year: '2-digit' }) : '—');
  $: chatFiles = Object.values($transfers)
    .filter((x) => (group ? x.group === conv.groupId : x.peer === conv.peerId && !x.group))
    .sort((a, b) => b.created - a.created);
</script>

<aside class="info" transition:fly={{ x: 40, duration: 280, opacity: 0 }}>
  <div class="info-head">
    <span class="eyebrow grow">{$t('chats.spaceSettings')}</span>
    <button class="icon" aria-label={$t('chats.closeInfo')} on:click={() => dispatch('close')}><Icon name="close" size={17} /></button>
  </div>
  <div class="profile">
    <Avatar id={conv.peerId || conv.id} name={conv.name} size={76} />
    <h2>{conv.name}</h2>
    <p class="small muted">{#if conv.kind === 'direct'}{conv.online ? $t('common.online') : $t('common.offline')}{:else}{$t('chats.members', { n: conv.members })}{#if conv.myRole} · {$t('chats.youAre', { role: roleLabel(conv.myRole) })}{/if}{/if}</p>
  </div>

  <div class="facts">
    <div><span class="faint small">{$t('chats.factMessages')}</span><b>{msgCount}{$hasOlder ? '+' : ''}</b></div>
    <div><span class="faint small">{$t('chats.factSince')}</span><b>{sinceDate(firstTs)}</b></div>
    <button class="fact" on:click={() => activeId.set(SAVED_ID)} disabled={!savedHere} title={$t('saved.open')}><span class="faint small">{$t('saved.short')}</span><b>{savedHere}</b></button>
  </div>

  <div class="block">
    <div class="eyebrow">{$t('files.title', { n: chatFiles.length })}</div>
    {#if chatFiles.length}
      <div class="files">
        {#each chatFiles as f (f.id)}
          <div class="frow">
            <Icon name={f.dir === 'out' ? 'upload' : 'download'} size={15} />
            <div class="grow fmeta">
              <span class="fname" title={f.name}>{f.name}</span>
              <span class="faint small">{bytes(f.size)} · {f.group && f.dir === 'out' && f.members && !f.fetching && f.state === 'active' ? $t('files.delivered', { n: f.delivered || 0, of: f.members }) : f.state === 'active' ? Math.round((f.done / Math.max(1, f.size)) * 100) + '%' : f.state === 'offered' && f.dir === 'out' ? $t(f.group ? 'files.st.shared' : 'files.st.waitingAccept') : $t('files.st.' + f.state)}</span>
            </div>
            {#if f.state === 'done' && f.dir === 'in'}<button class="icon tiny" title={$t('files.open')} aria-label={$t('files.open')} on:click={() => openFile(f)}><Icon name="external" size={14} /></button>{/if}
            {#if f.path && f.state !== 'failed' && (f.dir === 'out' || f.state === 'done')}<button class="icon tiny" title={$t('files.showInFolder')} aria-label={$t('files.showInFolder')} on:click={() => showInFolder(f.id)}><Icon name="folder" size={14} /></button>{/if}
          </div>
        {/each}
      </div>
    {:else}
      <p class="small faint">{$t('files.none')}</p>
    {/if}
  </div>

  {#if group}<GroupInvites {conv} {roles} />{/if}

    {#if group}<GroupRequests {conv} />{/if}
    {#if group}<GroupRoles {conv} on:roles={(e) => (roles = e.detail)} />{/if}

    <div class="block">
      <div class="eyebrow">{$t('chats.membersCount', { n: members.length })}</div>
      <div class="members">
        {#each members as m (m.peerId)}
          <div class="member">
            <div class="row">
              <span class="av" title={m.online ? $t('common.online') : $t('common.offline')}>
                <Avatar id={m.peerId} name={nameOfMember(m)} size={30} />
                <span class="presence" class:on={m.online}></span>
              </span>
              <span class="grow nm">{nameOfMember(m)}</span>
              {#if m.timeout}<span class="to-chip" title={$t('chats.to.until', { when: untilText(m.timeout) })}><Icon name="clock" size={12} /></span>{/if}
              {#if m.roles && m.roles.length}
                {#if roleColor(m.role)}<span class="role-dot" style:background={roleColor(m.role)}></span>{/if}
                <select value={m.role} disabled={busy === m.peerId} on:change={(e) => setRole(m, e.target.value)}>
                  {#each m.roles as r}<option value={r}>{roleLabel(r)}</option>{/each}
                  {#if !m.roles.includes(m.role)}<option value={m.role}>{roleLabel(m.role, m.roleTitle)}</option>{/if}
                </select>
              {:else}
                <span class="chip" style:color={roleColor(m.role) || null}>{roleLabel(m.role, m.roleTitle)}</span>
              {/if}
            </div>
            {#if m.canTransfer || m.canKick || m.canBan || m.canTimeout}
              <div class="row m-acts">
                {#if m.canTimeout}
                  {#if m.timeout}<button class="ghost small" disabled={busy === m.peerId} on:click={() => timeout(m, 0)}>{$t('chats.to.lift')}</button>
                  {:else}<button class="ghost small" disabled={busy === m.peerId} on:click={() => (timing = timing === m.peerId ? '' : m.peerId)}>{$t('chats.to.give')}</button>{/if}
                {/if}
                {#if m.canTransfer}<button class="ghost small" disabled={busy === m.peerId} on:click={() => makeOwner(m)}>{$t('chats.makeOwner')}</button>{/if}
                {#if m.canKick}<button class="ghost small" disabled={busy === m.peerId} on:click={() => kick(m, false)}>{$t('common.remove')}</button>{/if}
                {#if m.canBan}<button class="ghost small danger" disabled={busy === m.peerId} on:click={() => kick(m, true)}>{$t('chats.ban')}</button>{/if}
              </div>
              {#if timing === m.peerId}
                <div class="row m-acts to-opts">
                  {#each TIMEOUTS as min}<button class="ghost small" on:click={() => timeout(m, min)}>{timeoutLabel(min)}</button>{/each}
                </div>
              {/if}
            {/if}
          </div>
        {/each}
      </div>
    </div>

    {#if conv.can.manage}
      <div class="block">
        <form class="row" on:submit|preventDefault={addMember}>
          <input class="grow" placeholder={$t('chats.addByKey')} bind:value={newMemberKey} spellcheck="false" />
          <button type="submit" disabled={!newMemberKey.trim() || busy === 'add'}>{$t('common.add')}</button>
        </form>
        <p class="small faint">{$t('chats.addHint')}</p>
      </div>
    {/if}
    {#if group && (conv.can.manage || conv.can.roles)}<ModLog {conv} {roles} />{/if}
    {#if conv.myRole !== 'owner'}
      <button class="danger with-icon wide" on:click={leave}><Icon name="logout" size={16} /> {$t('chats.leaveGroup')}</button>
    {:else}
      <button class="danger with-icon wide" on:click={deleteGroup}><Icon name="trash" size={16} /> {$t('chats.deleteGroup')}</button>
      <p class="small faint">{$t('chats.ownerLeaveHint')}</p>
    {/if}
  {#if error}<p class="error small">{error}</p>{/if}
</aside>

<style>
  .role-dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
  .to-chip { display: inline-flex; color: var(--warn, #f5a524); }
  .to-opts { flex-wrap: wrap; }
</style>
