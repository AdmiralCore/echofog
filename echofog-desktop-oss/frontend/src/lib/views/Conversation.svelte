<script>
  import { afterUpdate } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { chats, messages, chatError, chatNote, membersVersion, activeId, loadChats, send, deleteContact, leaveGroup } from '../stores/chats.js';
  import { isCallActive, startCall, stageExpanded, peerInfo } from '../stores/call.store.js';
  import { time, short } from '../format.js';
  import Avatar from '../components/ui/Avatar.svelte';
  import Icon from '../components/ui/Icon.svelte';

  export let conv;

  const RANK = { owner: 100, admin: 50, moderator: 25, member: 10, subscriber: 10, restricted: 1 };
  const ASSIGNABLE = ['admin', 'moderator', 'member', 'restricted'];
  const FRESH_CALL_SECONDS = 15 * 60;

  let text = '';
  let sending = false;
  let scroller;
  let lastCount = 0;
  let info = false;
  let members = [];
  let invite = '';
  let infoError = '';
  let copied = '';
  let newMemberKey = '';
  let busyMember = '';

  let shownId = '';
  $: if (conv.id !== shownId) { shownId = conv.id; info = false; } // another chat closes the panel; a refresh of this one does not
  $: myRank = RANK[conv.myRole] || 0;
  $: canManage = conv.kind === 'group' && myRank >= RANK.admin;
  $: if (info && conv.kind === 'group') { void $membersVersion; loadMembers(); }

  afterUpdate(() => {
    if (scroller && $messages.length !== lastCount) {
      lastCount = $messages.length;
      scroller.scrollTop = scroller.scrollHeight;
    }
  });

  async function submit() {
    if (!text.trim() || sending) return;
    sending = true;
    const ok = await send(text);
    if (ok) text = '';
    sending = false;
  }

  function keydown(e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  }

  async function call(video) {
    if (conv.kind === 'direct') {
      startCall({ alias: conv.name, peerId: conv.peerId, pubKey: conv.pubKey, chatId: conv.id }, video);
      return;
    }
    try {
      // join a call somebody started a moment ago, otherwise start one: that rings the others
      let c = await App.ActiveGroupCall(conv.groupId);
      const fresh = c && Date.now() / 1000 - c.startedAt < FRESH_CALL_SECONDS;
      if (!c || !fresh) c = await App.StartGroupCall(conv.groupId);
      startCall({ alias: conv.name, callId: c.callId, coordinator: c.coordinator, chatId: conv.id }, video);
    } catch (err) {
      chatError.set(errText(err, 'Could not start the call'));
    }
  }

  async function loadMembers() {
    try {
      members = (await App.ListGroupMembers(conv.groupId)) || [];
      invite = await App.GetGroupInvite(conv.groupId);
      infoError = '';
    } catch (err) {
      infoError = errText(err);
    }
  }

  function toggleInfo() {
    info = !info;
    infoError = '';
  }

  async function doCopy(what, value) {
    if (await copy(value)) {
      copied = what;
      setTimeout(() => (copied = ''), 1500);
    }
  }

  async function removeContact() {
    if (!window.confirm(`Delete ${conv.name} from contacts? Message history stays on this computer.`)) return;
    try { await deleteContact(conv); } catch (err) { infoError = errText(err); }
  }

  async function memberAction(id, fn) {
    busyMember = id;
    infoError = '';
    try {
      await fn();
      await loadMembers();
    } catch (err) {
      infoError = errText(err);
    } finally {
      busyMember = '';
    }
  }

  function makeOwner(m) {
    if (!window.confirm(`Make ${senderName(m.peerId)} the owner of ${conv.name}? You will become an admin and cannot undo this yourself.`)) return;
    memberAction(m.peerId, async () => { await App.TransferGroupOwnership(conv.groupId, m.peerId); await loadChats(); });
  }

  async function deleteGroup() {
    if (!window.confirm(`Delete ${conv.name} for everybody? Its messages are removed from every member's computer.`)) return;
    try { await App.DeleteGroup(conv.groupId); await loadChats(); activeId.set(''); } catch (err) { infoError = errText(err); }
  }

  const setRole = (m, role) => memberAction(m.peerId, () => App.SetGroupRole(conv.groupId, m.peerId, role));

  function kick(m, ban) {
    const what = ban ? 'Ban' : 'Remove';
    if (!window.confirm(`${what} ${senderName(m.peerId)} from the group?`)) return;
    memberAction(m.peerId, () => App.RemoveGroupMember(conv.groupId, m.peerId, ban));
  }

  async function addMember() {
    const key = newMemberKey.trim();
    if (!key) return;
    await memberAction('add', () => App.AddGroupMember(conv.groupId, key));
    if (!infoError) newMemberKey = '';
  }

  async function leave() {
    if (!window.confirm(`Leave ${conv.name}? You will not receive its messages any more.`)) return;
    try { await leaveGroup(conv); } catch (err) { infoError = errText(err); }
  }

  $: names = Object.fromEntries($chats.filter((c) => c.kind === 'direct').map((c) => [c.peerId, c.name]));
  $: senderName = (id) => names[id] || (id ? short(id, 4) : '');
  const canModerate = (m) => !m.isMe && myRank >= RANK.moderator && myRank > (RANK[m.role] || 0);
  const canAssign = (m) => !m.isMe && canManage && myRank > (RANK[m.role] || 0);
  const assignable = (m) => ASSIGNABLE.filter((r) => RANK[r] < myRank || r === m.role);
</script>

<div class="conv">
  <header class="row">
    <Avatar id={conv.peerId || conv.id} name={conv.name} size={38} />
    <div class="grow title">
      <h3>{conv.name}</h3>
      <p class="small" class:online={conv.kind === 'direct' && conv.online}>
        {#if conv.kind === 'direct'}{conv.online ? 'online' : 'offline'}{:else}{conv.groupKind === 'channel' ? 'channel' : 'group'} · {conv.members} member{conv.members === 1 ? '' : 's'}{/if}
      </p>
    </div>
    {#if $isCallActive && $peerInfo.chatId === conv.id && !$stageExpanded}
      <button class="primary" on:click={() => stageExpanded.set(true)}>Show call</button>
    {/if}
    <button class="icon" title="Call" aria-label="Call" on:click={() => call(false)} disabled={$isCallActive}><Icon name="phone" size={18} /></button>
    <button class="icon" title="Video call" aria-label="Video call" on:click={() => call(true)} disabled={$isCallActive}><Icon name="video" size={19} /></button>
    <button class="icon" class:on={info} title="Info" aria-label="Info" on:click={toggleInfo}><Icon name="info" size={19} /></button>
  </header>

  {#if info}
    <div class="info stack tight">
      {#if conv.kind === 'direct'}
        <div class="small muted">Key</div>
        <div class="row"><span class="mono grow">{conv.pubKey || 'not known yet'}</span>
          <button class="ghost" on:click={() => doCopy('key', conv.pubKey)} disabled={!conv.pubKey}>{copied === 'key' ? 'Copied' : 'Copy'}</button></div>
        <div><button class="danger" on:click={removeContact}>Delete contact</button></div>
      {:else}
        <div class="small muted">Invite (share it with people you want in the group)</div>
        <div class="row"><span class="mono grow clip">{invite}</span>
          <button class="ghost" on:click={() => doCopy('invite', invite)} disabled={!invite}>{copied === 'invite' ? 'Copied' : 'Copy'}</button></div>

        <div class="small muted">Members</div>
        {#each members as m (m.peerId)}
          <div class="member row small">
            <span class="grow">{m.isMe ? 'you' : senderName(m.peerId)}</span>
            {#if canAssign(m)}
              <select value={m.role} disabled={busyMember === m.peerId} on:change={(e) => setRole(m, e.target.value)}>
                {#each assignable(m) as r}<option value={r}>{r}</option>{/each}
                {#if !ASSIGNABLE.includes(m.role)}<option value={m.role}>{m.role}</option>{/if}
              </select>
            {:else}
              <span class="muted">{m.role}</span>
            {/if}
            {#if conv.myRole === 'owner' && !m.isMe}
              <button class="ghost" disabled={busyMember === m.peerId} on:click={() => makeOwner(m)}>Make owner</button>
            {/if}
            {#if canModerate(m)}
              <button class="ghost" disabled={busyMember === m.peerId} on:click={() => kick(m, false)}>Remove</button>
              {#if myRank >= RANK.admin}<button class="ghost danger" disabled={busyMember === m.peerId} on:click={() => kick(m, true)}>Ban</button>{/if}
            {/if}
          </div>
        {/each}

        {#if canManage}
          <form class="row" on:submit|preventDefault={addMember}>
            <input class="grow" placeholder="Add a contact by key" bind:value={newMemberKey} spellcheck="false" />
            <button type="submit" disabled={!newMemberKey.trim() || busyMember === 'add'}>Add</button>
          </form>
          <p class="small faint">A person you add sees the group at once only if they have you as a contact; otherwise send them the invite.</p>
        {/if}
        {#if conv.myRole !== 'owner'}
          <div><button class="danger" on:click={leave}>Leave group</button></div>
        {:else}
          <div><button class="danger" on:click={deleteGroup}>Delete group for everyone</button></div>
          <p class="small faint">To leave instead, make another member the owner first.</p>
        {/if}
      {/if}
      {#if infoError}<p class="error small">{infoError}</p>{/if}
    </div>
  {/if}

  <div class="messages" bind:this={scroller}>
    {#each $messages as m (m.id)}
      {#if m.system}
        <div class="system small faint">{m.text}, {time(m.ts)}</div>
      {:else}
        <div class="msg" class:mine={m.mine} class:pending={m.pending}>
          {#if conv.kind === 'group' && !m.mine}<div class="sender small">{senderName(m.sender)}</div>{/if}
          <div class="text">{m.text}</div>
          <div class="meta small">
            {time(m.ts)}
            {#if m.failed}<span class="error">, not sent</span>{:else if m.pending}<span class="warn">, waiting for delivery</span>{/if}
          </div>
        </div>
      {/if}
    {:else}
      <p class="small muted empty">No messages yet.</p>
    {/each}
  </div>

  {#if $chatError}<p class="error small bar">{$chatError}</p>{/if}
  {#if $chatNote}<p class="muted small bar">{$chatNote}</p>{/if}

  {#if conv.readOnly}
    <p class="small muted bar">Only admins can write in this channel.</p>
  {:else}
    <form class="composer" on:submit|preventDefault={submit}>
      <div class="field row">
        <textarea class="grow" rows="1" placeholder="Write a message…" bind:value={text} on:keydown={keydown} />
        <button class="send" type="submit" title="Send" aria-label="Send" data-silent disabled={sending || !text.trim()}><Icon name="send" size={17} stroke={2} /></button>
      </div>
    </form>
  {/if}
</div>

<style>
  .conv { flex: 1; min-height: 0; display: flex; flex-direction: column; }
  header { padding: 12px 16px 12px 18px; gap: 12px; border-bottom: 1px solid var(--line-soft); background: color-mix(in srgb, var(--panel) 40%, var(--bg)); }
  header .icon { width: 38px; height: 38px; }
  .title p { color: var(--faint); }
  .title p.online { color: var(--ok); }
  .info { padding: 14px 18px; border-bottom: 1px solid var(--line-soft); background: var(--panel); max-height: 45%; overflow-y: auto; animation: fade-up 0.15s ease-out; }
  .clip { max-height: 3.2em; overflow: hidden; }
  .member { padding: 3px 0; }
  .member select { width: auto; padding: 3px 8px; }
  .member button { padding: 3px 10px; }
  .messages { flex: 1; overflow-y: auto; padding: 18px 20px; display: flex; flex-direction: column; gap: 4px; }
  .msg {
    max-width: min(72%, 620px); align-self: flex-start;
    background: var(--raised); border: 1px solid var(--line-soft);
    border-radius: 16px 16px 16px 6px; padding: 7px 12px 6px;
    animation: fade-up 0.16s ease-out;
  }
  .msg.mine {
    align-self: flex-end; border: none;
    background: var(--accent);
    color: #fff; border-radius: 16px 16px 6px 16px;
  }
  .msg.mine .meta { color: rgba(255, 255, 255, 0.65); }
  .msg.mine .meta :global(.warn) { color: #ffe1a6; }
  .msg.pending { opacity: .7; }
  .text { white-space: pre-wrap; word-wrap: break-word; line-height: 1.45; }
  .sender { color: var(--accent); font-weight: 600; margin-bottom: 1px; }
  .meta { color: var(--faint); text-align: right; font-size: 10.5px; margin-top: 1px; }
  .system { align-self: center; padding: 3px 10px; border-radius: 999px; background: var(--raised); margin: 6px 0; }
  .empty { margin: auto; color: var(--faint); }
  .bar { padding: 6px 20px; }
  .composer { padding: 10px 16px 14px; }
  .field {
    align-items: flex-end; gap: 6px; padding: 5px 5px 5px 14px;
    background: var(--raised); border: 1px solid var(--line); border-radius: 18px;
    transition: border-color 0.15s, box-shadow 0.15s;
  }
  .field:focus-within { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
  .field textarea { resize: none; max-height: 140px; border: none; background: transparent; padding: 7px 0; box-shadow: none; }
  .field textarea:focus { box-shadow: none; }
  .send {
    width: 36px; height: 36px; padding: 0; border-radius: 50%; border: none; flex: none;
    display: grid; place-items: center; color: #fff;
    background: linear-gradient(135deg, var(--accent), var(--accent-2));
  }
  .send:disabled { opacity: .35; }
  .send:hover:not(:disabled) { filter: brightness(1.1); background: linear-gradient(135deg, var(--accent), var(--accent-2)); }
</style>
