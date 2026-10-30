<script>
  // "Open an EchoFog link": takes any link the app knows (see lib/links.js), says what it is and
  // offers what can be done with it. Opened with Ctrl+L, by pasting a link anywhere outside a text
  // field, from the menus, or by a clicked echofog:// link that started or reached the app.
  import { tick } from 'svelte';
  import { fade, scale } from 'svelte/transition';
  import { App, errText } from '../api.js';
  import { openLink, classifyLink } from '../links.js';
  import { addContact, joinGroup, loadChats, openChat } from '../stores/chats.js';
  import { view } from '../stores/nav.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  let text = '';
  let input;
  let name = '';
  let busy = '';
  let error = '';
  let done = '';
  let invite = null; // what the core read from an echofog://join link

  $: if ($openLink) start($openLink.text);
  async function start(initial) {
    text = initial || '';
    name = '';
    error = '';
    done = '';
    await tick();
    input?.focus();
    if (!text) input?.select();
  }
  const close = () => openLink.set(null);

  $: link = classifyLink(text);
  $: readInvite(link && link.kind === 'join' ? link.text : '');
  async function readInvite(s) {
    invite = null;
    if (!s) return;
    try {
      const info = await App.ParseInvite(s);
      if (s === text.trim()) invite = info;
    } catch (err) {
      const msg = errText(err);
      const net = /network "([^"]*)"/.exec(msg);
      error = /newer version/.test(msg) ? $t('access.tooNew') : net ? $t('access.otherNet', { net: net[1] }) : msg;
    }
  }
  $: if (link?.kind === 'contact' && !name && link.name) name = link.name;
  $: hidden = link?.kind === 'join' && invite?.private;
  // a public node: by address, or the link of one
  $: node = link?.kind === 'node' || (link?.kind === 'join' && invite && !invite.private);
  $: peerId = link?.peerId || invite?.peerId || '';

  async function act(id, fn, then) {
    busy = id;
    error = '';
    done = '';
    try {
      await fn();
      if (then) then();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }
  const useAsExit = () => act('exit', () => App.StartVPNVia(text.trim()), () => { view.set('proxy'); close(); });
  const joinHidden = () => act('join', () => App.JoinInvite(text.trim()), () => (done = $t('links.joined')));
  const addRelay = () => act('relay', () => App.AddRelay(text.trim()), () => (done = $t('links.relayAdded')));
  const addBootstrap = () => act('boot', () => App.AddBootstrapNode(text.trim()), () => (done = $t('links.bootAdded')));
  const addPerson = () => act('contact', () => addContact(link.key, name.trim()), () => { view.set('chats'); close(); });
  const joinTheGroup = () => act('group', () => joinGroup(text.trim()), () => { view.set('chats'); close(); });

  // a group's file (.fogzip): its invitation and history, sealed with a password told apart
  let file = '';
  let filePass = '';
  async function pickFile() {
    error = '';
    try {
      const p = await App.GroupFilePick();
      if (p) { file = p; filePass = ''; }
    } catch (err) { error = errText(err); }
  }
  let fileGroup = '';
  const openFile = () => act('file', async () => {
    const info = await App.GroupFileOpen(file, filePass);
    fileGroup = info.group.id;
    await loadChats();
  }, async () => { view.set('chats'); close(); file = ''; await openChat('g:' + fileGroup); });
  const fileName = (p) => p.split(/[\\/]/).pop();

  function onKey(e) {
    if (!$openLink) return;
    if (e.key === 'Escape') { e.preventDefault(); close(); }
  }
  const short = (id) => (id ? id.slice(0, 8) + '…' + id.slice(-4) : '');
</script>

<svelte:window on:keydown={onKey} />

{#if $openLink}
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" transition:fade={{ duration: 140 }} on:click|self={close}>
    <div class="dlg" role="dialog" aria-modal="true" aria-labelledby="ol-title" transition:scale={{ start: 0.95, duration: 180 }}>
      <div class="head">
        <span class="ico"><Icon name="link" size={18} /></span>
        <h3 id="ol-title" class="grow">{$t('links.title')}</h3>
        <button class="icon tiny" aria-label={$t('common.close')} on:click={close}><Icon name="close" size={15} stroke={2.2} /></button>
      </div>

      <textarea bind:this={input} bind:value={text} rows="3" spellcheck="false" placeholder={$t('links.placeholder')} on:input={() => { error = ''; done = ''; }} />

      {#if text.trim() && !link}
        <p class="small error">{$t('links.unknown')}</p>
      {:else if link}
        <div class="what">
          {#if link.kind === 'join' && !invite && !error}
            <span class="small faint">{$t('links.reading')}</span>
          {:else if hidden}
            <b>{$t('links.kind.hidden')}</b>
            <span class="small faint">{$t('links.hiddenNote')} · <span class="mono">{short(peerId)}</span></span>
          {:else if node}
            <b>{invite?.server || link.kind === 'node' ? $t('links.kind.server') : $t('links.kind.node')}</b>
            <span class="small faint">
              <span class="mono">{short(peerId)}</span>
              {#if invite?.sni}{' · '}{$t('links.cover', { sni: invite.sni })}{/if}
              {#if invite?.relays}{' · '}{$t('links.viaRelay', { n: invite.relays })}{/if}
            </span>
          {:else if link.kind === 'contact'}
            <b>{$t('links.kind.contact')}</b>
            <span class="small faint mono">{short(link.key)}</span>
          {:else if link.kind === 'group'}
            <b>{$t('links.kind.group')}</b>
            <span class="small faint">{$t('links.groupNote')}</span>
          {/if}
        </div>

        <div class="actions">
          {#if hidden}
            <button class="primary" disabled={!!busy} on:click={joinHidden}>{#if busy === 'join'}<span class="spinner light"></span>{/if}{$t('links.join')}</button>
            <button disabled={!!busy} on:click={useAsExit}>{#if busy === 'exit'}<span class="spinner"></span>{/if}{$t('links.joinAndVpn')}</button>
          {:else if node}
            <button class="primary" disabled={!!busy} on:click={useAsExit}>{#if busy === 'exit'}<span class="spinner light"></span>{/if}<Icon name="shield" size={16} /> {$t('links.useAsExit')}</button>
            <button disabled={!!busy} on:click={addRelay}>{#if busy === 'relay'}<span class="spinner"></span>{/if}<Icon name="relay" size={16} /> {$t('links.addRelay')}</button>
            <button disabled={!!busy} on:click={addBootstrap}>{#if busy === 'boot'}<span class="spinner"></span>{/if}<Icon name="globe" size={16} /> {$t('links.addBootstrap')}</button>
          {:else if link.kind === 'contact'}
            <input bind:value={name} maxlength="64" placeholder={$t('links.contactName')} on:keydown={(e) => e.key === 'Enter' && name.trim() && addPerson()} />
            <button class="primary" disabled={!!busy || !name.trim()} on:click={addPerson}>{#if busy === 'contact'}<span class="spinner light"></span>{/if}<Icon name="userPlus" size={16} /> {$t('links.addContact')}</button>
          {:else if link.kind === 'group'}
            <button class="primary" disabled={!!busy} on:click={joinTheGroup}>{#if busy === 'group'}<span class="spinner light"></span>{/if}<Icon name="users" size={16} /> {$t('links.joinGroup')}</button>
          {/if}
        </div>
      {/if}

      <div class="file-row">
        {#if file}
          <span class="small grow mono clip" title={file}><Icon name="file" size={13} /> {fileName(file)}</span>
          <input type="password" bind:value={filePass} placeholder={$t('links.filePass')} on:keydown={(e) => e.key === 'Enter' && filePass && openFile()} />
          <button class="primary" disabled={!!busy || !filePass} on:click={openFile}>{#if busy === 'file'}<span class="spinner light"></span>{/if}{$t('links.fileOpen')}</button>
        {:else}
          <button class="ghost small with-icon" on:click={pickFile}><Icon name="folder" size={14} /> {$t('links.filePick')}</button>
        {/if}
      </div>

      {#if error}<p class="small error">{error}</p>{/if}
      {#if done}<p class="small ok"><Icon name="check" size={14} stroke={2.4} /> {done}</p>{/if}
      <p class="small faint hint">{$t('links.hint')}</p>
    </div>
  </div>
{/if}

<style>
  .backdrop { position: fixed; inset: 0; z-index: 10001; display: grid; place-items: center; padding: 16px; background: var(--scrim); backdrop-filter: blur(6px); }
  .dlg {
    width: min(480px, 100%); display: flex; flex-direction: column; gap: 12px; padding: 18px;
    border-radius: 20px; background: var(--panel); border: 1px solid var(--line); box-shadow: var(--shadow-lg);
  }
  .head { display: flex; align-items: center; gap: 10px; }
  .head h3 { margin: 0; font-size: 16px; }
  .ico { width: 34px; height: 34px; border-radius: 10px; display: grid; place-items: center; background: var(--accent-soft); color: var(--accent); flex: none; }
  .icon.tiny { width: 28px; height: 28px; border-radius: 8px; }
  textarea { resize: none; font-family: var(--mono, ui-monospace, monospace); font-size: 12.5px; line-height: 1.45; word-break: break-all; }
  .what { display: flex; flex-direction: column; gap: 2px; padding: 10px 12px; border-radius: 12px; background: var(--raised); }
  .actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .file-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .file-row input { flex: 1; min-width: 140px; }
  .file-row .clip { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 220px; }
  .actions button { display: inline-flex; align-items: center; gap: 7px; }
  .actions input { flex: 1; min-width: 160px; }
  .ok { color: var(--ok); display: flex; align-items: center; gap: 6px; margin: 0; }
  .error { margin: 0; }
  .hint { margin: 0; line-height: 1.4; }
</style>
