<script>
  import { onMount } from 'svelte';
  import QRCode from 'qrcode';
  import { me, profile } from '../stores/session.js';
  import { chats, activeId, loadChats, openChat, addContact, createGroup, joinGroup } from '../stores/chats.js';
  import { isCallActive, peerInfo, stageExpanded } from '../stores/call.store.js';
  import CallStage from '../components/call/CallStage.svelte';
  import { errText, copy } from '../api.js';
  import { when } from '../format.js';
  import { makeContactLink, parseContactInput, displayName, currentDisplayName } from '../contact.js';
  import Conversation from './Conversation.svelte';
  import QRScan from './QRScan.svelte';
  import Avatar from '../components/ui/Avatar.svelte';
  import Icon from '../components/ui/Icon.svelte';

  let panel = ''; // '' | 'contact' | 'group' | 'join' | 'me'
  let contactInput = '';
  let name = '';
  let groupKind = 'group';
  let invite = '';
  let busy = false;
  let error = '';
  let copied = '';
  let scanning = false;
  let qr = '';

  // the chat list can be hidden so the conversation uses the full width; the choice is remembered
  let listCollapsed = false;
  try { listCollapsed = localStorage.getItem('echofog.chatsCollapsed') === '1'; } catch (_) {}
  function toggleList() {
    listCollapsed = !listCollapsed;
    try { localStorage.setItem('echofog.chatsCollapsed', listCollapsed ? '1' : '0'); } catch (_) {}
  }

  $: sorted = [...$chats].sort((a, b) => (b.ts || 0) - (a.ts || 0));
  $: active = $chats.find((c) => c.id === $activeId);
  $: showCall = $isCallActive && !!active && $peerInfo.chatId === $activeId && $stageExpanded;
  let maximized = false;
  $: if (!showCall) maximized = false;
  $: parsed = parseContactInput(contactInput);
  $: myLink = $me.pubKey ? makeContactLink(currentDisplayName($profile), $me.pubKey) : '';

  onMount(loadChats);

  async function toggle(p) {
    panel = panel === p ? '' : p;
    contactInput = name = invite = error = '';
    scanning = false;
    if (panel === 'me' && myLink) {
      qr = await QRCode.toDataURL(myLink, { margin: 1, width: 200, color: { dark: '#000000', light: '#ffffff' } });
    }
  }

  async function run(fn) {
    busy = true;
    error = '';
    try {
      await fn();
      panel = '';
      contactInput = name = invite = '';
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  function submitContact() {
    if (!parsed) { error = 'This is neither a contact link nor a 64-character key.'; return; }
    const finalName = name.trim() || parsed.name;
    if (!finalName) { error = 'Enter a name for the contact.'; return; }
    run(() => addContact(parsed.key, finalName));
  }

  function scanned(e) {
    scanning = false;
    contactInput = e.detail;
    if (!parseContactInput(contactInput)) error = 'This code is not an EchoFog contact.';
  }

  async function copyText(what, text) {
    if (await copy(text)) {
      copied = what;
      setTimeout(() => (copied = ''), 1500);
    }
  }
</script>

<div class="chats">
  {#if !listCollapsed}
  <aside>
    <div class="head row">
      <h2 class="grow">Chats</h2>
      <button class="icon" title="Hide the chat list" aria-label="Hide the chat list" on:click={toggleList}><Icon name="chevronLeft" size={18} /></button>
      <button class="icon" class:on={panel === 'contact'} title="Add contact" aria-label="Add contact" on:click={() => toggle('contact')}><Icon name="userPlus" size={18} /></button>
      <button class="icon" class:on={panel === 'group'} title="New group" aria-label="New group" on:click={() => toggle('group')}><Icon name="users" size={18} /></button>
      <button class="icon" class:on={panel === 'join'} title="Join by invite" aria-label="Join by invite" on:click={() => toggle('join')}><Icon name="link" size={18} /></button>
      <button class="icon" class:on={panel === 'me'} title="My contact" aria-label="My contact" on:click={() => toggle('me')}><Icon name="qr" size={18} /></button>
    </div>
    {#if panel}
      <p class="panel-title small">{({ contact: 'Add a contact', group: 'New group or channel', join: 'Join by invite', me: 'Your contact' })[panel]}</p>
    {/if}

    {#if panel === 'contact'}
      {#if scanning}
        <QRScan on:scan={scanned} on:close={() => (scanning = false)} />
      {:else}
        <form class="form stack tight" on:submit|preventDefault={submitContact}>
          <textarea rows="3" placeholder="Paste a contact link or key" bind:value={contactInput} spellcheck="false" />
          <input placeholder={parsed?.name ? `Name (${parsed.name})` : 'Name'} bind:value={name} />
          <div class="row">
            <button class="primary grow" type="submit" disabled={busy || !contactInput.trim()}>Add</button>
            <button type="button" on:click={() => (scanning = true)}>Scan code</button>
          </div>
        </form>
      {/if}
    {:else if panel === 'group'}
      <form class="form stack tight" on:submit|preventDefault={() => run(() => createGroup(name, groupKind))}>
        <input placeholder="Group name" bind:value={name} />
        <select bind:value={groupKind}>
          <option value="group">Group: everyone can write</option>
          <option value="channel">Channel: only admins write</option>
        </select>
        <button class="primary" type="submit" disabled={busy || !name.trim()}>Create</button>
      </form>
    {:else if panel === 'join'}
      <form class="form stack tight" on:submit|preventDefault={() => run(() => joinGroup(invite))}>
        <textarea rows="3" placeholder="Paste the invite" bind:value={invite} spellcheck="false" />
        <button class="primary" type="submit" disabled={busy || !invite.trim()}>Join</button>
      </form>
    {:else if panel === 'me'}
      <div class="form stack tight">
        <p class="small muted">Show this code or send the link to people so they can add you. It carries your name ({currentDisplayName($profile)}) and your key. Change the name in Settings.</p>
        {#if qr}<img class="qr" src={qr} alt="Your contact code" />{/if}
        <div class="row">
          <button class="grow" on:click={() => copyText('link', myLink)}>{copied === 'link' ? 'Copied' : 'Copy link'}</button>
          <button class="grow" on:click={() => copyText('key', $me.pubKey)}>{copied === 'key' ? 'Copied' : 'Copy key'}</button>
        </div>
      </div>
    {/if}
    {#if error}<p class="error small form">{error}</p>{/if}

    <div class="list">
      {#each sorted as c (c.id)}
        <button class="ghost conv" class:sel={c.id === $activeId} on:click={() => openChat(c.id)}>
          <div class="avatar">
            <Avatar id={c.peerId || c.id} name={c.name} size={40} />
            {#if c.kind === 'direct'}<span class="presence" class:on={c.online} />{:else}<span class="kind">#</span>{/if}
          </div>
          <div class="body">
            <div class="row">
              <span class="grow name">{c.name}</span>
              {#if c.ts}<span class="time">{when(c.ts)}</span>{/if}
            </div>
            <div class="row">
              <span class="grow last">{c.last || (c.kind === 'group' ? `${c.members} member${c.members === 1 ? '' : 's'}` : 'No messages yet')}</span>
              {#if c.unread}<span class="badge">{c.unread}</span>{/if}
            </div>
          </div>
        </button>
      {:else}
        <p class="small muted empty">No chats yet. Add a contact from their link or code, or join a group with an invite.</p>
      {/each}
    </div>
  </aside>
  {/if}

  <section>
    {#if listCollapsed}
      <button class="show-list" title="Show the chat list" aria-label="Show the chat list" on:click={toggleList}><Icon name="chevronRight" size={18} /></button>
    {/if}
    {#if showCall}
      <div class="call" class:full={maximized}><CallStage title={active.name} bind:maximized /></div>
    {/if}
    {#if active}
      {#if !maximized}<Conversation conv={active} />{/if}
    {:else}
      <div class="placeholder muted">Select a chat</div>
    {/if}
  </section>
</div>

<style>
  .chats { display: flex; height: 100%; }
  aside { width: 320px; flex: none; display: flex; flex-direction: column; border-right: 1px solid var(--line-soft); min-height: 0; background: color-mix(in srgb, var(--panel) 55%, var(--bg)); }
  .head { padding: 16px 12px 10px 18px; gap: 2px; }
  .panel-title { padding: 0 18px 6px; color: var(--faint); text-transform: uppercase; letter-spacing: 0.06em; font-weight: 600; font-size: 11px; }
  .form { padding: 4px 14px 14px; animation: fade-up 0.15s ease-out; }
  .qr { width: 168px; height: 168px; border-radius: 12px; align-self: center; padding: 6px; background: #fff; }
  .list { flex: 1; overflow-y: auto; padding: 4px 8px 8px; }
  .conv { width: 100%; text-align: left; display: flex; gap: 12px; align-items: center; padding: 9px 10px; border-radius: 12px; margin-bottom: 2px; }
  .conv.sel { background: var(--accent-soft); }
  .conv.sel:hover:not(:disabled) { background: var(--accent-soft); }
  .avatar { position: relative; flex: none; }
  .presence { position: absolute; right: -1px; bottom: -1px; width: 12px; height: 12px; border-radius: 50%; background: var(--faint); border: 2.5px solid var(--panel); }
  .presence.on { background: var(--ok); }
  .kind { position: absolute; right: -3px; bottom: -3px; width: 17px; height: 17px; border-radius: 6px; background: var(--raised); border: 2px solid var(--panel); color: var(--muted); font-size: 10px; font-weight: 700; display: grid; place-items: center; }
  .body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
  .name { overflow: hidden; text-overflow: ellipsis; font-weight: 600; color: var(--text); }
  .time { color: var(--faint); font-size: 11.5px; font-weight: 400; }
  .last { color: var(--muted); overflow: hidden; text-overflow: ellipsis; font-size: 13px; font-weight: 400; }
  .empty { padding: 16px 12px; }
  section { flex: 1; min-width: 0; display: flex; flex-direction: column; position: relative; }
  .show-list {
    position: absolute; z-index: 5; top: 12px; left: 12px;
    width: 30px; height: 30px; border-radius: 9px; display: flex; align-items: center; justify-content: center;
    background: var(--raised); border: 1px solid var(--line-soft); color: var(--muted); box-shadow: var(--shadow, 0 4px 14px rgba(0,0,0,0.25));
  }
  .show-list:hover { color: var(--text); }
  .call { height: 58%; min-height: 300px; flex: none; background: #000; }
  .call.full { height: 100%; flex: 1; }
  .placeholder { flex: 1; display: grid; place-items: center; color: var(--faint); }
</style>
