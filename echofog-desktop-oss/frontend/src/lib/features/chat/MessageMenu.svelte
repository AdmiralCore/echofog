<script>
  import { createEventDispatcher } from 'svelte';
  import { react, pinMessage, deleteMessage } from '../../stores/chats.js';
  import { savedIds, toggleSaved, saveAsFile } from '../../stores/saved.js';
  import { copy } from '../../api.js';
  import { bytes } from '../../format.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Icon from '../../components/ui/Icon.svelte';
  import Popover from '../../components/ui/Popover.svelte';
  import { useChat, stateOf } from './context.js';

  /** What can be done with one message; m.can (from the core) says what is allowed. */
  export let conv;
  export let m;
  export let anchor;

  const QUICK = ['👍', '❤️', '😂', '😮', '😢', '🔥'];
  const chat = useChat();
  const { nameOf, quoteOf, authorName } = chat;
  const dispatch = createEventDispatcher();
  const close = () => dispatch('close');

  let details = false;
  let note = '';
  $: inThread = !!m.thread;

  const reacted = (emoji) => m.reactions.some((r) => r.emoji === emoji && r.mine);
  function toggleReaction(emoji) {
    close();
    react(m.id, emoji, !reacted(emoji));
  }
  async function doCopy() {
    if (await copy(m.file ? m.file.name : m.text)) {
      note = $t('common.copied');
      setTimeout(close, 600);
    }
  }
  async function save() {
    const on = await toggleSaved(conv, m, $nameOf(m.sender));
    if (on !== null) {
      note = on ? $t('saved.added') : $t('saved.removed');
      setTimeout(close, 700);
    }
  }
  async function saveFile() {
    close();
    await saveAsFile(m, conv.name);
  }
  function pin() {
    close();
    pinMessage(m.id, !m.pinned);
  }
  async function remove() {
    close();
    const msg = m.mine ? $t('chats.confirm.deleteMsg') : $t('chats.confirm.deleteTheirs', { name: $nameOf(m.sender) });
    if (!(await ask({ message: msg, confirm: $t('chats.deleteForAll'), danger: true }))) return;
    chat.dropDraftOf(m.id);
    deleteMessage(m.id);
  }
  function act(fn) {
    close();
    fn(m);
  }
</script>

<Popover {anchor} width={270} label={$t('chats.msgActions')} on:close={close}>
  <div class="menu">
    {#if details}
      <div class="details">
        <div><span class="faint small">{$t('chats.det.sent')}</span><span>{chat.fullTime(m.ts)}</span></div>
        <div><span class="faint small">{$t('chats.det.from')}</span><span>{$authorName(m)}</span></div>
        {#if m.edited}<div><span class="faint small">{$t('chats.det.edited')}</span><span>{chat.fullTime(m.editedAt)}</span></div>{/if}
        {#if m.mine && conv.kind === 'direct'}<div><span class="faint small">{$t('chats.det.state')}</span><span>{$t('chats.st.' + stateOf(m))}</span></div>{/if}
        {#if m.file}<div><span class="faint small">{$t('chats.det.file')}</span><span>{m.file.name} · {bytes(m.file.size)}</span></div>{/if}
        <div><span class="faint small">{$t('chats.det.id')}</span><span class="mono tiny-id">{m.id}</span></div>
        <p class="small faint">{$t('chats.det.e2e')}</p>
      </div>
    {:else}
      {#if m.can.react}
        <div class="quick">
          {#each QUICK as emoji}
            <button class="qr" class:on={reacted(emoji)} title={emoji} on:click={() => toggleReaction(emoji)}>{emoji}</button>
          {/each}
        </div>
      {/if}
      {#if m.can.reply && !inThread}<button class="item" on:click={() => act(chat.startReply)}><Icon name="reply" size={16} /> {$t('chats.reply')}</button>{/if}
      {#if m.can.reply && !inThread}<button class="item" on:click={() => act(chat.showThread)}><Icon name="message" size={16} /> {m.replies ? $t('chats.comments', { n: m.replies }) : $t('chats.comment')}</button>{/if}
      {#if m.can.edit}<button class="item" on:click={() => act(chat.startEdit)}><Icon name="edit" size={16} /> {$t('chats.edit')}</button>{/if}
      {#if m.can.pin}<button class="item" on:click={pin}><Icon name="pin" size={16} /> {m.pinned ? $t('chats.unpin') : $t('chats.pin')}</button>{/if}
      {#if !m.deleted}
        <button class="item" on:click={doCopy}><Icon name="copy" size={16} /> {m.file ? $t('chats.copyName') : $t('chats.copyText')}</button>
        {#if !inThread}<button class="item" on:click={save}><Icon name="bookmark" size={16} /> {$savedIds.has(conv.id + '/' + m.id) ? $t('saved.remove') : $t('saved.add')}</button>{/if}
        {#if !m.file && m.text}<button class="item" on:click={saveFile}><Icon name="download" size={16} /> {$t('chats.saveAsFile')}</button>{/if}
      {/if}
      <button class="item" on:click={() => (details = true)}><Icon name="info" size={16} /> {$t('chats.details')}</button>
      {#if m.can.delete}<button class="item danger-item" on:click={remove}><Icon name="trash" size={16} /> {$t('chats.deleteForAll')}</button>{/if}
      {#if conv.kind === 'direct' && !conv.upgraded && !m.deleted}<p class="small faint menu-hint">{$t('chats.oldPeerHint')}</p>{/if}
      {#if note}<p class="small note-ok">{note}</p>{/if}
    {/if}
  </div>
</Popover>

<style>
  .menu { display: flex; flex-direction: column; padding: 6px; gap: 2px; }
  .item { display: flex; align-items: center; gap: 10px; width: 100%; padding: 8px 10px; border: none; border-radius: 10px; background: transparent; text-align: left; font-weight: 500; }
  .item:hover:not(:disabled) { background: var(--hover); border: none; }
  .note-ok { color: var(--ok); padding: 2px 10px 4px; }
  .details { display: flex; flex-direction: column; gap: 9px; padding: 6px 8px 8px; }
  .details > div { display: flex; flex-direction: column; gap: 1px; }
  .tiny-id { font-size: 11px; word-break: break-all; }
  .quick { display: flex; justify-content: space-between; gap: 2px; padding: 2px 4px 6px; margin-bottom: 2px; border-bottom: 1px solid var(--line-soft); }
  .qr { width: 36px; height: 36px; padding: 0; border: none; border-radius: 50%; background: transparent; font-size: 19px; line-height: 1; display: grid; place-items: center; }
  .qr:hover:not(:disabled) { background: var(--hover); border: none; transform: scale(1.12); }
  .qr.on { background: var(--accent-soft); }
  .danger-item { color: var(--danger); }
  .menu-hint { padding: 4px 10px 2px; line-height: 1.4; }
</style>
