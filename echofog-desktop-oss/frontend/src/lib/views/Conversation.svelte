<script>
  import { afterUpdate, createEventDispatcher, tick } from 'svelte';
  import { fly } from 'svelte/transition';
  import { App, errText, copy } from '../api.js';
  import { chats, messages, chatError, chatNote, membersVersion, activeId, loadChats, send, deleteContact, leaveGroup } from '../stores/chats.js';
  import { isCallActive, startCall, stageExpanded, peerInfo } from '../stores/call.store.js';
  import { time, short } from '../format.js';
  import Avatar from '../components/ui/Avatar.svelte';
  import Icon from '../components/ui/Icon.svelte';

  export let conv;
  /** back: show a back arrow (narrow window, the list is hidden); indent: leave room for the show-list button */
  export let back = false;
  export let indent = false;

  const dispatch = createEventDispatcher();
  let input;

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
      // one new message glides into view; opening a chat jumps straight to the end
      const smooth = lastCount > 0 && $messages.length - lastCount <= 2 && $messages.length > lastCount;
      lastCount = $messages.length;
      scroller.scrollTo({ top: scroller.scrollHeight, behavior: smooth ? 'smooth' : 'auto' });
    }
  });

  async function submit() {
    if (!text.trim() || sending) return;
    sending = true;
    const ok = await send(text);
    if (ok) text = '';
    sending = false;
    await tick();
    grow();
  }

  // the composer grows with its text up to a few lines
  function grow() {
    if (!input) return;
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 160) + 'px';
  }

  // consecutive messages from the same person within a few minutes form one visual group
  const GROUP_GAP = 5 * 60 * 1000;
  const sameGroup = (a, b) => a && b && !a.system && !b.system && a.mine === b.mine && a.sender === b.sender && Math.abs((b.ts || 0) - (a.ts || 0)) < GROUP_GAP;

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
  <div class="main">
    <header class:indent>
      {#if back}
        <button class="icon" title="Back to chats" aria-label="Back to chats" on:click={() => dispatch('back')}><Icon name="arrowLeft" size={19} /></button>
      {/if}
      <button class="who" on:click={toggleInfo} title="Chat info">
        <div class="av">
          <Avatar id={conv.peerId || conv.id} name={conv.name} size={40} />
          {#if conv.kind === 'direct'}<span class="presence" class:on={conv.online}></span>{/if}
        </div>
        <div class="title">
          <h3>{conv.name}</h3>
          <p class="small" class:online={conv.kind === 'direct' && conv.online}>
            {#if conv.kind === 'direct'}{conv.online ? 'online' : 'offline'}{:else}{conv.groupKind === 'channel' ? 'channel' : 'group'} · {conv.members} member{conv.members === 1 ? '' : 's'}{/if}
          </p>
        </div>
      </button>
      {#if $isCallActive && $peerInfo.chatId === conv.id && !$stageExpanded}
        <button class="primary small with-icon" on:click={() => stageExpanded.set(true)}><Icon name="phone" size={14} /> Show call</button>
      {/if}
      <div class="tools">
        <button class="icon" title="Call" aria-label="Call" on:click={() => call(false)} disabled={$isCallActive}><Icon name="phone" size={18} /></button>
        <button class="icon" title="Video call" aria-label="Video call" on:click={() => call(true)} disabled={$isCallActive}><Icon name="video" size={19} /></button>
        <button class="icon" class:on={info} title="Info" aria-label="Info" on:click={toggleInfo}><Icon name="info" size={19} /></button>
      </div>
    </header>

    <div class="messages" bind:this={scroller}>
      {#each $messages as m, i (m.id)}
        {#if m.system}
          <div class="system small">{m.text} · {time(m.ts)}</div>
        {:else}
          {@const first = !sameGroup($messages[i - 1], m)}
          {@const last = !sameGroup(m, $messages[i + 1])}
          <div class="msg" class:mine={m.mine} class:pending={m.pending} class:failed={m.failed} class:first class:last>
            {#if conv.kind === 'group' && !m.mine && first}<div class="sender small">{senderName(m.sender)}</div>{/if}
            <span class="text">{m.text}</span>
            <span class="meta">
              {time(m.ts)}
              {#if m.mine}
                {#if m.failed}<span class="st err" title="Not sent">!</span>
                {:else if m.pending}<span class="st" title="Waiting for delivery"><Icon name="check" size={13} stroke={2.4} /></span>
                {:else}<span class="st" title="Delivered"><Icon name="checks" size={14} stroke={2.2} /></span>{/if}
              {/if}
            </span>
          </div>
          {#if m.failed}<div class="fail-note small">not sent</div>{:else if m.pending && last}<div class="fail-note small pend">waiting for delivery</div>{/if}
        {/if}
      {:else}
        <div class="empty">
          <Avatar id={conv.peerId || conv.id} name={conv.name} size={64} />
          <p class="small muted">No messages yet. Say hi to {conv.name}.</p>
        </div>
      {/each}
    </div>

    {#if $chatError}<p class="error small bar" transition:fly={{ y: 6, duration: 200 }}>{$chatError}</p>{/if}
    {#if $chatNote}<p class="muted small bar" transition:fly={{ y: 6, duration: 200 }}>{$chatNote}</p>{/if}

    {#if conv.readOnly}
      <p class="small muted bar ro"><Icon name="lock" size={14} /> Only admins can write in this channel.</p>
    {:else}
      <form class="composer" on:submit|preventDefault={submit}>
        <div class="field">
          <textarea bind:this={input} rows="1" placeholder="Write a message…" bind:value={text} on:keydown={keydown} on:input={grow} />
          <button class="send" class:ready={text.trim()} type="submit" title="Send" aria-label="Send" data-silent disabled={sending || !text.trim()}><Icon name="send" size={17} stroke={2} /></button>
        </div>
      </form>
    {/if}
  </div>

  {#if info}
    <aside class="info" transition:fly={{ x: 40, duration: 280, opacity: 0 }}>
      <div class="info-head">
        <span class="eyebrow grow">{conv.kind === 'direct' ? 'Contact' : conv.groupKind === 'channel' ? 'Channel' : 'Group'}</span>
        <button class="icon" aria-label="Close info" on:click={toggleInfo}><Icon name="close" size={17} /></button>
      </div>
      <div class="profile">
        <Avatar id={conv.peerId || conv.id} name={conv.name} size={76} />
        <h2>{conv.name}</h2>
        <p class="small muted">{#if conv.kind === 'direct'}{conv.online ? 'online' : 'offline'}{:else}{conv.members} member{conv.members === 1 ? '' : 's'}{#if conv.myRole} · you are {conv.myRole}{/if}{/if}</p>
      </div>

      {#if conv.kind === 'direct'}
        <div class="block">
          <div class="eyebrow">Key</div>
          <div class="copybox">
            <span class="mono grow">{conv.pubKey || 'not known yet'}</span>
            <button class="icon" title="Copy" on:click={() => doCopy('key', conv.pubKey)} disabled={!conv.pubKey}><Icon name={copied === 'key' ? 'check' : 'copy'} size={16} /></button>
          </div>
        </div>
        <button class="danger with-icon wide" on:click={removeContact}><Icon name="trash" size={16} /> Delete contact</button>
      {:else}
        <div class="block">
          <div class="eyebrow">Invite</div>
          <div class="copybox">
            <span class="mono grow clip">{invite}</span>
            <button class="icon" title="Copy invite" on:click={() => doCopy('invite', invite)} disabled={!invite}><Icon name={copied === 'invite' ? 'check' : 'copy'} size={16} /></button>
          </div>
          <p class="small faint">Share it with people you want in the group.</p>
        </div>

        <div class="block">
          <div class="eyebrow">Members · {members.length}</div>
          <div class="members">
            {#each members as m (m.peerId)}
              <div class="member">
                <div class="row">
                  <Avatar id={m.peerId} name={m.isMe ? 'you' : senderName(m.peerId)} size={30} />
                  <span class="grow nm">{m.isMe ? 'you' : senderName(m.peerId)}</span>
                  {#if canAssign(m)}
                    <select value={m.role} disabled={busyMember === m.peerId} on:change={(e) => setRole(m, e.target.value)}>
                      {#each assignable(m) as r}<option value={r}>{r}</option>{/each}
                      {#if !ASSIGNABLE.includes(m.role)}<option value={m.role}>{m.role}</option>{/if}
                    </select>
                  {:else}
                    <span class="chip">{m.role}</span>
                  {/if}
                </div>
                {#if (conv.myRole === 'owner' && !m.isMe) || canModerate(m)}
                  <div class="row m-acts">
                    {#if conv.myRole === 'owner' && !m.isMe}
                      <button class="ghost small" disabled={busyMember === m.peerId} on:click={() => makeOwner(m)}>Make owner</button>
                    {/if}
                    {#if canModerate(m)}
                      <button class="ghost small" disabled={busyMember === m.peerId} on:click={() => kick(m, false)}>Remove</button>
                      {#if myRank >= RANK.admin}<button class="ghost small danger" disabled={busyMember === m.peerId} on:click={() => kick(m, true)}>Ban</button>{/if}
                    {/if}
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        </div>

        {#if canManage}
          <div class="block">
            <form class="row" on:submit|preventDefault={addMember}>
              <input class="grow" placeholder="Add a contact by key" bind:value={newMemberKey} spellcheck="false" />
              <button type="submit" disabled={!newMemberKey.trim() || busyMember === 'add'}>Add</button>
            </form>
            <p class="small faint">A person you add sees the group at once only if they have you as a contact; otherwise send them the invite.</p>
          </div>
        {/if}
        {#if conv.myRole !== 'owner'}
          <button class="danger with-icon wide" on:click={leave}><Icon name="logout" size={16} /> Leave group</button>
        {:else}
          <button class="danger with-icon wide" on:click={deleteGroup}><Icon name="trash" size={16} /> Delete group for everyone</button>
          <p class="small faint">To leave instead, make another member the owner first.</p>
        {/if}
      {/if}
      {#if infoError}<p class="error small">{infoError}</p>{/if}
    </aside>
  {/if}
</div>

<style>
  .conv { flex: 1; min-height: 0; display: flex; position: relative; }
  .main { flex: 1; min-width: 0; display: flex; flex-direction: column; }

  header {
    display: flex; align-items: center; gap: 8px; padding: 10px 14px 10px 14px; min-height: 64px;
    border-bottom: 1px solid var(--line-soft); background: var(--glass); backdrop-filter: blur(16px) saturate(1.3);
    position: relative; z-index: 2;
  }
  header.indent { padding-left: 58px; }
  .who {
    flex: 1; min-width: 0; display: flex; align-items: center; gap: 12px; padding: 4px 8px 4px 4px;
    background: transparent; border: none; text-align: left; font-weight: inherit; border-radius: 14px;
  }
  .who:hover:not(:disabled) { background: var(--hover); border: none; }
  .who:active:not(:disabled) { transform: scale(0.99); }
  .av { position: relative; flex: none; }
  .presence { position: absolute; right: -1px; bottom: -1px; width: 12px; height: 12px; border-radius: 50%; background: var(--faint); border: 2.5px solid var(--panel); }
  .presence.on { background: var(--ok); }
  .title { min-width: 0; }
  .title h3 { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .title p { color: var(--faint); }
  .title p.online { color: var(--ok); }
  .tools { display: flex; gap: 2px; }
  .tools .icon { width: 38px; height: 38px; }

  .messages { flex: 1; overflow-y: auto; padding: 18px clamp(12px, 3vw, 28px) 10px; display: flex; flex-direction: column; }
  .msg {
    position: relative; max-width: min(76%, 640px); align-self: flex-start;
    background: var(--bubble-in); border: 1px solid var(--line-soft); box-shadow: var(--shadow-sm);
    border-radius: 6px 18px 18px 6px; padding: 7px 12px 7px; margin-top: 2px;
    animation: msg-in 0.32s var(--spring) both; transform-origin: bottom left;
  }
  .msg.first { border-top-left-radius: 18px; margin-top: 10px; }
  .msg.last { border-bottom-left-radius: 18px; }
  .msg.first.last { border-bottom-left-radius: 6px; }
  .msg.mine {
    align-self: flex-end; border: none; transform-origin: bottom right;
    background: linear-gradient(135deg, var(--accent), var(--accent-2));
    color: #fff; border-radius: 18px 6px 6px 18px;
  }
  .msg.mine.first { border-top-right-radius: 18px; }
  .msg.mine.last { border-bottom-right-radius: 18px; }
  .msg.mine.first.last { border-bottom-right-radius: 6px; }
  .msg.pending { opacity: 0.72; }
  .msg.failed { background: var(--danger); }
  @keyframes msg-in { from { opacity: 0; transform: translateY(8px) scale(0.96); } to { opacity: 1; transform: none; } }
  .text { white-space: pre-wrap; word-wrap: break-word; overflow-wrap: anywhere; line-height: 1.45; }
  .sender { color: var(--accent); font-weight: 650; margin-bottom: 1px; }
  .meta {
    float: right; display: inline-flex; align-items: center; gap: 3px; margin: 6px 0 -3px 10px;
    color: var(--faint); font-size: 10.5px; font-variant-numeric: tabular-nums; line-height: 1;
  }
  .msg.mine .meta { color: rgba(255, 255, 255, 0.72); }
  .st { display: inline-flex; }
  .st.err { font-weight: 800; color: #fff; }
  .fail-note { align-self: flex-end; color: var(--danger); font-size: 11px; margin: 2px 4px 0; }
  .fail-note.pend { color: var(--faint); }
  .system { align-self: center; padding: 4px 12px; border-radius: 999px; background: var(--raised); color: var(--muted); margin: 14px 0 6px; font-size: 12px; }
  .empty { margin: auto; display: flex; flex-direction: column; align-items: center; gap: 12px; animation: fade-up 0.4s var(--ease); }

  .bar { padding: 6px 20px; }
  .ro { display: flex; align-items: center; gap: 8px; justify-content: center; padding: 14px; border-top: 1px solid var(--line-soft); }
  .composer { padding: 8px clamp(10px, 2.5vw, 20px) 14px; }
  .field {
    display: flex; align-items: flex-end; gap: 6px; padding: 5px 5px 5px 16px;
    background: var(--panel); border: 1px solid var(--line); border-radius: 22px; box-shadow: var(--shadow-sm);
    transition: border-color var(--dur), box-shadow var(--dur);
  }
  .field:focus-within { border-color: var(--accent); box-shadow: 0 0 0 4px var(--accent-soft); }
  .field textarea { flex: 1; resize: none; max-height: 160px; border: none; background: transparent; padding: 8px 0; box-shadow: none; min-height: 38px; }
  .field textarea:focus { box-shadow: none; background: transparent; }
  .send {
    width: 38px; height: 38px; padding: 0; border-radius: 50%; border: none; flex: none;
    display: grid; place-items: center; color: var(--faint); background: var(--raised);
    transform: scale(0.9);
  }
  .send.ready { color: #fff; background: linear-gradient(135deg, var(--accent), var(--accent-2)); box-shadow: var(--glow); transform: scale(1); }
  .send:disabled { opacity: 1; }
  .send.ready:hover:not(:disabled) { filter: brightness(1.1); transform: scale(1.06) rotate(-8deg); background: linear-gradient(135deg, var(--accent), var(--accent-2)); }

  .info {
    width: 320px; flex: none; overflow-y: auto; display: flex; flex-direction: column; gap: 16px;
    padding: 12px 16px 20px; border-left: 1px solid var(--line-soft); background: var(--panel);
  }
  @media (max-width: 1100px) {
    .info { position: absolute; right: 0; top: 0; bottom: 0; z-index: 10; box-shadow: var(--shadow-lg); max-width: 100%; }
  }
  .info-head { display: flex; align-items: center; min-height: 40px; }
  .profile { display: flex; flex-direction: column; align-items: center; gap: 6px; text-align: center; padding: 4px 0 6px; }
  .profile h2 { margin-top: 6px; font-size: 18px; }
  .block { display: flex; flex-direction: column; gap: 8px; }
  .copybox { display: flex; align-items: center; gap: 6px; padding: 8px 6px 8px 12px; border-radius: 12px; background: var(--raised); }
  .clip { max-height: 3.4em; overflow: hidden; }
  .members { display: flex; flex-direction: column; gap: 2px; }
  .member { padding: 6px 6px; border-radius: 12px; transition: background var(--dur); }
  .member:hover { background: var(--hover); }
  .member .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 550; }
  .member select { width: auto; padding: 4px 28px 4px 10px; font-size: 12px; border-radius: 9px; background-position: right 8px center; }
  .m-acts { padding: 4px 0 0 38px; gap: 4px; flex-wrap: wrap; }
  .wide { justify-content: center; width: 100%; }
</style>

