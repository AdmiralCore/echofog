<script>
  import { createEventDispatcher } from 'svelte';
  import { fly } from 'svelte/transition';
  import { App, errText, copy } from '../../api.js';
  import { messages, hasOlder, membersVersion, activeId, loadChats, deleteContact, leaveGroup, blockPeer, unblockPeer } from '../../stores/chats.js';
  import { transfers, openFile, showInFolder } from '../../stores/files.js';
  import { savedIds, SAVED_ID } from '../../stores/saved.js';
  import { bytes } from '../../format.js';
  import { t, intlLocale } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import { useChat } from './context.js';

  /** The side panel about the chat: facts, files, and the contact or the group (its members). What I
   *  may do to each member comes from the core (canKick, canBan, roles, canTransfer). */
  export let conv;

  const chat = useChat();
  const { nameOf } = chat;
  const dispatch = createEventDispatcher();

  let members = [];
  let invite = '';
  let error = '';
  let copied = '';
  let newMemberKey = '';
  let busy = ''; // the member being changed, or 'add'
  let reqBusy = false;

  $: group = conv.kind === 'group';
  $: if (group) { void $membersVersion; loadMembers(conv.groupId); }

  async function loadMembers(id) {
    try {
      members = (await App.ListGroupMembers(id)) || [];
      invite = conv.can.invite ? await App.GetGroupInvite(id) : '';
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }

  async function doCopy(what, value) {
    if (await copy(value)) {
      copied = what;
      setTimeout(() => (copied = ''), 1500);
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
  async function req(fn) {
    reqBusy = true;
    error = '';
    try { await fn(); } catch (err) { error = errText(err); } finally { reqBusy = false; }
  }
  async function block() {
    if (!(await ask({ message: $t('chats.confirm.block', { name: conv.name }), confirm: $t('chats.block'), danger: true }))) return;
    await req(() => blockPeer(conv));
  }
  async function removeContact() {
    if (!(await ask({ message: $t('chats.confirm.deleteContact', { name: conv.name }), confirm: $t('common.delete'), danger: true }))) return;
    try { await deleteContact(conv); } catch (err) { error = errText(err); }
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
    <span class="eyebrow grow">{conv.kind === 'direct' ? $t('chats.contact') : conv.groupKind === 'channel' ? $t('chats.channel') : $t('chats.group')}</span>
    <button class="icon" aria-label={$t('chats.closeInfo')} on:click={() => dispatch('close')}><Icon name="close" size={17} /></button>
  </div>
  <div class="profile">
    <Avatar id={conv.peerId || conv.id} name={conv.name} size={76} />
    <h2>{conv.name}</h2>
    <p class="small muted">{#if conv.kind === 'direct'}{conv.online ? $t('common.online') : $t('common.offline')}{:else}{$t('chats.members', { n: conv.members })}{#if conv.myRole} · {$t('chats.youAre', { role: $t('chats.role.' + conv.myRole) })}{/if}{/if}</p>
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

  {#if conv.kind === 'direct'}
    <div class="block">
      <div class="eyebrow">{$t('chats.key')}</div>
      <div class="copybox">
        <span class="mono grow">{conv.pubKey || $t('chats.keyUnknown')}</span>
        <button class="icon" title={$t('common.copy')} on:click={() => doCopy('key', conv.pubKey)} disabled={!conv.pubKey}><Icon name={copied === 'key' ? 'check' : 'copy'} size={16} /></button>
      </div>
      <p class="small faint">{$t('chats.e2eContact')}</p>
    </div>

    {#if conv.blocked}
      <button class="with-icon wide" on:click={() => req(() => unblockPeer(conv))} disabled={reqBusy}><Icon name="lockOpen" size={16} /> {$t('chats.unblock')}</button>
    {:else}
      <button class="danger with-icon wide" on:click={block} disabled={reqBusy}><Icon name="lock" size={16} /> {$t('chats.block')}</button>
      <p class="small faint">{$t('chats.blockHint')}</p>
    {/if}
    {#if !conv.request}
      <button class="danger with-icon wide" on:click={removeContact}><Icon name="trash" size={16} /> {$t('chats.deleteContact')}</button>
    {/if}
  {:else}
    {#if invite}
      <div class="block">
        <div class="eyebrow">{$t('chats.invite')}</div>
        <div class="copybox">
          <span class="mono grow clip">{invite}</span>
          <button class="icon" title={$t('chats.copyInvite')} on:click={() => doCopy('invite', invite)}><Icon name={copied === 'invite' ? 'check' : 'copy'} size={16} /></button>
        </div>
        <p class="small faint">{$t('chats.inviteHint')}</p>
      </div>
    {/if}

    <div class="block">
      <div class="eyebrow">{$t('chats.membersCount', { n: members.length })}</div>
      <div class="members">
        {#each members as m (m.peerId)}
          <div class="member">
            <div class="row">
              <Avatar id={m.peerId} name={nameOfMember(m)} size={30} />
              <span class="grow nm">{nameOfMember(m)}</span>
              {#if m.roles && m.roles.length}
                <select value={m.role} disabled={busy === m.peerId} on:change={(e) => setRole(m, e.target.value)}>
                  {#each m.roles as r}<option value={r}>{$t('chats.role.' + r)}</option>{/each}
                  {#if !m.roles.includes(m.role)}<option value={m.role}>{$t('chats.role.' + m.role)}</option>{/if}
                </select>
              {:else}
                <span class="chip">{$t('chats.role.' + m.role)}</span>
              {/if}
            </div>
            {#if m.canTransfer || m.canKick || m.canBan}
              <div class="row m-acts">
                {#if m.canTransfer}<button class="ghost small" disabled={busy === m.peerId} on:click={() => makeOwner(m)}>{$t('chats.makeOwner')}</button>{/if}
                {#if m.canKick}<button class="ghost small" disabled={busy === m.peerId} on:click={() => kick(m, false)}>{$t('common.remove')}</button>{/if}
                {#if m.canBan}<button class="ghost small danger" disabled={busy === m.peerId} on:click={() => kick(m, true)}>{$t('chats.ban')}</button>{/if}
              </div>
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
    {#if conv.myRole !== 'owner'}
      <button class="danger with-icon wide" on:click={leave}><Icon name="logout" size={16} /> {$t('chats.leaveGroup')}</button>
    {:else}
      <button class="danger with-icon wide" on:click={deleteGroup}><Icon name="trash" size={16} /> {$t('chats.deleteGroup')}</button>
      <p class="small faint">{$t('chats.ownerLeaveHint')}</p>
    {/if}
  {/if}
  {#if error}<p class="error small">{error}</p>{/if}
</aside>
