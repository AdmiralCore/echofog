<script>
  import { createEventDispatcher, onMount, tick } from 'svelte';
  import { fly } from 'svelte/transition';
  import { App, errText, copy, openURL } from '../../api.js';
  import { loadChats, deleteContact, blockPeer, unblockPeer, muted, toggleMuted } from '../../stores/chats.js';
  import { isCallActive } from '../../stores/call.store.js';
  import { bytes, short, when } from '../../format.js';
  import { linkHost } from '../../linkify.js';
  import { t, lang } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';
  import { useChat } from './context.js';

  /** A contact's profile: the name and picture I gave them (seen on this device only), quick
   *  actions, what our chat holds (media, files, links), and the rarer things under "More". */
  export let conv;

  const dispatch = createEventDispatcher();
  const chat = useChat();

  let error = '';
  let busy = false;

  // ---- the name and the picture ------------------------------------------------------------------
  let naming = false;
  let nameText = '';
  let nameInput;
  async function startNaming() {
    if (conv.request) return;
    naming = true;
    nameText = conv.name;
    await tick();
    nameInput?.select();
  }
  async function saveProfile(alias, avatar) {
    error = '';
    try {
      await App.SetContactProfile(conv.peerId, alias, avatar);
      await loadChats();
    } catch (err) {
      error = errText(err);
    }
  }
  async function saveName() {
    naming = false;
    const n = nameText.trim();
    if (n && n !== conv.name) await saveProfile(n, rawAvatar(conv.avatar));
  }
  const rawAvatar = (src) => (src ? src.slice(src.indexOf(',') + 1) : '');

  let picker;
  // a picture is made small (256 px, JPEG) before it is kept
  async function picked(e) {
    const file = e.target.files && e.target.files[0];
    e.target.value = '';
    if (!file) return;
    try {
      const img = await createImageBitmap(file);
      const size = 256;
      const c = document.createElement('canvas');
      c.width = c.height = size;
      const s = Math.min(img.width, img.height);
      c.getContext('2d').drawImage(img, (img.width - s) / 2, (img.height - s) / 2, s, s, 0, 0, size, size);
      const data = c.toDataURL('image/jpeg', 0.85);
      await saveProfile(conv.name, data.slice(data.indexOf(',') + 1));
    } catch (_) {
      error = $t('chats.err.picture');
    }
  }

  // ---- what the chat holds -----------------------------------------------------------------------
  let tab = 'media';
  let att = { media: [], files: [], links: [] };
  let loaded = false;
  async function loadAttachments() {
    try {
      att = await App.ChatAttachments(conv.ref);
    } catch (_) { /* an older core */ }
    loaded = true;
  }
  onMount(loadAttachments);

  function goTo(m) {
    dispatch('close');
    chat.goTo(m.id || m.messageId);
  }

  // ---- more --------------------------------------------------------------------------------------
  let more = false;
  let copied = false;
  async function copyKey() {
    if (await copy(conv.pubKey)) {
      copied = true;
      setTimeout(() => (copied = false), 1500);
    }
  }
  async function run(fn) {
    busy = true;
    error = '';
    try { await fn(); } catch (err) { error = errText(err); } finally { busy = false; }
  }
  async function block() {
    if (!(await ask({ message: $t('chats.confirm.block', { name: conv.name }), confirm: $t('chats.block'), danger: true }))) return;
    run(() => blockPeer(conv));
  }
  async function removeContact() {
    if (!(await ask({ message: $t('chats.confirm.deleteContact', { name: conv.name }), confirm: $t('common.delete'), danger: true }))) return;
    run(() => deleteContact(conv));
  }
</script>

<aside class="info contact" transition:fly={{ x: 40, duration: 280, opacity: 0 }}>
  <div class="info-head">
    <span class="grow"></span>
    <button class="icon" aria-label={$t('chats.closeInfo')} on:click={() => dispatch('close')}><Icon name="close" size={17} /></button>
  </div>

  <div class="profile">
    <button class="pic" title={$t('chats.setPicture')} aria-label={$t('chats.setPicture')} on:click={() => picker.click()} disabled={conv.request}>
      <Avatar id={conv.peerId} name={conv.name} src={conv.avatar || ''} size={84} />
      {#if !conv.request}<span class="pic-edit"><Icon name="camera" size={15} /></span>{/if}
    </button>
    <input bind:this={picker} type="file" accept="image/*" hidden on:change={picked} />
    {#if naming}
      <form class="name-form" on:submit|preventDefault={saveName}>
        <input bind:this={nameInput} bind:value={nameText} maxlength="64" on:blur={saveName} on:keydown={(e) => e.key === 'Escape' && (naming = false)} />
      </form>
    {:else}
      <button class="name" on:click={startNaming} disabled={conv.request} title={$t('chats.rename')}>
        <h2>{conv.name}</h2>{#if !conv.request}<Icon name="edit" size={15} />{/if}
      </button>
    {/if}
    <p class="small muted">{short(conv.peerId, 4)} · {conv.online ? $t('common.online') : $t('common.offline')}</p>
    {#if conv.avatar && !conv.request}<button class="link-btn small" on:click={() => saveProfile(conv.name, '')}>{$t('chats.removePicture')}</button>{/if}
  </div>

  <div class="quick">
    <button class="q" title={$t('chats.openChat')} aria-label={$t('chats.openChat')} on:click={() => dispatch('close')}><Icon name="message" size={18} /></button>
    <button class="q" title={$t('chats.call')} aria-label={$t('chats.call')} disabled={$isCallActive || !conv.can.call} on:click={() => dispatch('call', false)}><Icon name="phone" size={18} /></button>
    <button class="q" title={$t('chats.videoCall')} aria-label={$t('chats.videoCall')} disabled={$isCallActive || !conv.can.call} on:click={() => dispatch('call', true)}><Icon name="video" size={18} /></button>
    <button class="q" class:on={$muted.has(conv.id)} title={$muted.has(conv.id) ? $t('chats.unmute') : $t('chats.mute')} aria-label={$muted.has(conv.id) ? $t('chats.unmute') : $t('chats.mute')} on:click={() => toggleMuted(conv.id)}><Icon name="bellOff" size={18} /></button>
  </div>

  <div class="tabs" role="tablist">
    <button role="tab" class:on={tab === 'media'} aria-selected={tab === 'media'} on:click={() => (tab = 'media')}>{$t('chats.tabMedia')}</button>
    <button role="tab" class:on={tab === 'files'} aria-selected={tab === 'files'} on:click={() => (tab = 'files')}>{$t('chats.tabFiles')}</button>
    <button role="tab" class:on={tab === 'links'} aria-selected={tab === 'links'} on:click={() => (tab = 'links')}>{$t('chats.tabLinks')}</button>
  </div>

  <div class="tab-body">
    {#if tab === 'media'}
      {#if att.media.length}
        <div class="grid">
          {#each att.media as m (m.id)}
            <button class="tile" title={m.file.name} on:click={() => goTo(m)}><Icon name="image" size={20} /><span class="tname">{m.file.name}</span></button>
          {/each}
        </div>
      {:else if loaded}<p class="small faint">{$t('chats.noMedia')}</p>{/if}
    {:else if tab === 'files'}
      {#each att.files as m (m.id)}
        <button class="frow" on:click={() => goTo(m)}>
          <Icon name="file" size={16} />
          <span class="grow fmeta"><span class="fname">{m.file.name}</span><span class="faint small">{bytes(m.file.size)} · {when(m.ts, $lang)}</span></span>
        </button>
      {:else}
        {#if loaded}<p class="small faint">{$t('files.none')}</p>{/if}
      {/each}
    {:else}
      {#each att.links as l, i (l.messageId + i)}
        <div class="lrow">
          <button class="grow lmain" on:click={() => openURL(l.url)} title={l.url}>
            <span class="fname">{linkHost(l.url)}</span><span class="faint small lurl">{l.url}</span>
          </button>
          <button class="icon tiny" title={$t('chats.showInChat')} aria-label={$t('chats.showInChat')} on:click={() => goTo(l)}><Icon name="message" size={14} /></button>
        </div>
      {:else}
        {#if loaded}<p class="small faint">{$t('chats.noLinks')}</p>{/if}
      {/each}
    {/if}
  </div>

  <div class="more">
    <button class="more-head" on:click={() => (more = !more)}><span class="eyebrow grow">{$t('chats.more')}</span><Icon name={more ? 'chevronUp' : 'chevronDown'} size={15} /></button>
    {#if more}
      <div class="block">
        <div class="eyebrow">{$t('chats.key')}</div>
        <div class="copybox">
          <span class="mono grow">{conv.pubKey || $t('chats.keyUnknown')}</span>
          <button class="icon" title={$t('common.copy')} on:click={copyKey} disabled={!conv.pubKey}><Icon name={copied ? 'check' : 'copy'} size={16} /></button>
        </div>
        <p class="small faint">{$t('chats.e2eContact')}</p>
      </div>
      {#if conv.blocked}
        <button class="with-icon wide" on:click={() => run(() => unblockPeer(conv))} disabled={busy}><Icon name="lockOpen" size={16} /> {$t('chats.unblock')}</button>
      {:else}
        <button class="danger with-icon wide" on:click={block} disabled={busy}><Icon name="lock" size={16} /> {$t('chats.block')}</button>
        <p class="small faint">{$t('chats.blockHint')}</p>
      {/if}
      {#if !conv.request}
        <button class="danger with-icon wide" on:click={removeContact} disabled={busy}><Icon name="trash" size={16} /> {$t('chats.deleteContact')}</button>
      {/if}
    {/if}
  </div>
  {#if error}<p class="error small">{error}</p>{/if}
</aside>

<style>
  .contact { gap: 14px; }
  .pic { position: relative; padding: 0; border: none; background: transparent; border-radius: 50%; }
  .pic:hover:not(:disabled) { background: transparent; border: none; filter: brightness(1.08); }
  .pic:disabled { opacity: 1; }
  .pic-edit { position: absolute; right: 0; bottom: 0; width: 28px; height: 28px; border-radius: 50%; display: grid; place-items: center; background: var(--panel); border: 2px solid var(--line); color: var(--muted); }
  .name { display: flex; align-items: center; gap: 6px; padding: 2px 6px; border: none; background: transparent; color: var(--text); font-weight: inherit; }
  .name:hover:not(:disabled) { background: var(--hover); border: none; }
  .name:disabled { opacity: 1; }
  .name h2 { margin: 0; font-size: 18px; }
  .name-form input { text-align: center; font-size: 16px; padding: 6px 10px; }
  .link-btn { border: none; background: transparent; color: var(--faint); padding: 0; text-decoration: underline; }
  .link-btn:hover:not(:disabled) { background: transparent; border: none; color: var(--text); }
  .quick { display: flex; justify-content: center; gap: 10px; }
  .q { width: 44px; height: 44px; border-radius: 14px; display: grid; place-items: center; padding: 0; background: var(--raised); border: none; color: var(--muted); }
  .q:hover:not(:disabled) { background: var(--hover); border: none; color: var(--text); }
  .q.on { color: var(--accent); background: var(--accent-soft); }
  .tabs { display: flex; gap: 2px; padding: 3px; border-radius: 12px; background: var(--raised); }
  .tabs button { flex: 1; padding: 6px 8px; border: none; border-radius: 9px; background: transparent; color: var(--muted); font-weight: 600; font-size: 13px; }
  .tabs button:hover:not(:disabled) { background: var(--hover); border: none; color: var(--text); }
  .tabs button.on { background: var(--panel); color: var(--text); box-shadow: var(--shadow-sm); }
  .tab-body { display: flex; flex-direction: column; gap: 2px; min-height: 80px; }
  .grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 4px; }
  .tile { aspect-ratio: 1; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 4px; padding: 6px; border: none; border-radius: 10px; background: var(--raised); color: var(--muted); }
  .tile:hover:not(:disabled) { background: var(--hover); border: none; color: var(--text); }
  .tname { font-size: 11px; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .frow { display: flex; align-items: center; gap: 8px; padding: 7px 8px; border: none; border-radius: 10px; background: transparent; color: var(--muted); text-align: left; font-weight: 400; }
  .frow:hover:not(:disabled) { background: var(--hover); border: none; }
  .fmeta { min-width: 0; display: flex; flex-direction: column; }
  .fname { color: var(--text); font-weight: 550; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }
  .lrow { display: flex; align-items: center; gap: 4px; border-radius: 10px; }
  .lrow:hover { background: var(--hover); }
  .lmain { min-width: 0; display: flex; flex-direction: column; align-items: flex-start; padding: 7px 8px; border: none; background: transparent; text-align: left; font-weight: 400; }
  .lmain:hover:not(:disabled) { background: transparent; border: none; }
  .lurl { max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .more { display: flex; flex-direction: column; gap: 10px; border-top: 1px solid var(--line-soft); padding-top: 8px; }
  .more-head { display: flex; align-items: center; padding: 4px 2px; border: none; background: transparent; color: var(--faint); }
  .more-head:hover:not(:disabled) { background: transparent; border: none; color: var(--text); }
</style>
