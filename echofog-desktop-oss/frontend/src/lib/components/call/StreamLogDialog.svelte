<script>
  /**
   * A stream log that just finished: its summary, and ways to keep it or hand it on — copy the
   * summary, save the whole log as a file, or send the file into a chat (the call's own group
   * chat comes first). Nothing leaves the device unless the user sends it.
   */
  import { createEventDispatcher } from 'svelte';
  import { App, errText, copy } from '../../api.js';
  import { chats } from '../../stores/chats.js';
  import { sendPaths } from '../../stores/files.js';
  import { summaryText } from '../../services/streamlog.js';
  import Icon from '../ui/Icon.svelte';
  import { t } from '../../i18n/index.js';

  export let log;

  const dispatch = createEventDispatcher();
  const close = () => dispatch('close');

  const text = summaryText(log);
  let error = '';
  let copied = false;
  let savedPath = '';
  let sending = false;
  let sentTo = '';

  $: targets = $chats.filter((c) => !c.blocked && !c.request && c.can?.files !== false && c.can?.send !== false);
  let chatId = log.chatId || '';
  $: if (!chatId && targets.length) chatId = targets[0].id;

  async function copySummary() {
    await copy(text);
    copied = true;
    setTimeout(() => (copied = false), 1500);
  }

  async function save() {
    error = '';
    try { savedPath = await App.SaveStreamLog(log.name, log.text); } catch (err) { error = errText(err); }
    return savedPath;
  }

  async function send() {
    const conv = targets.find((c) => c.id === chatId);
    if (!conv) return;
    sending = true;
    try {
      const path = savedPath || (await save());
      if (path) {
        await sendPaths(conv, [path]);
        sentTo = conv.name;
      }
    } catch (err) {
      error = errText(err);
    } finally {
      sending = false;
    }
  }

  function portal(node) {
    document.body.appendChild(node);
    return { destroy: () => node.remove() };
  }
</script>

<svelte:window on:keydown={(e) => e.key === 'Escape' && close()} />

<div class="layer" use:portal>
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" on:click={close}></div>
  <div class="dialog" role="dialog" aria-label={$t('call.log.title')}>
    <header>
      <div class="grow">
        <h2>{$t('call.log.title')}</h2>
        <p class="small muted">{$t('call.log.hint')}</p>
      </div>
      <button class="icon" title={$t('common.close')} aria-label={$t('common.close')} on:click={close}><Icon name="close" size={18} /></button>
    </header>

    <pre class="text mono">{text}</pre>
    <p class="small faint">{$t('call.log.inside', { s: log.summary.seconds, n: log.lines })}</p>
    {#if error}<p class="small error">{error}</p>{/if}

    <div class="row btns">
      <button class="with-icon" on:click={copySummary}><Icon name={copied ? 'check' : 'copy'} size={14} /> {copied ? $t('common.copied') : $t('call.log.copy')}</button>
      <button class="with-icon" on:click={save}><Icon name="download" size={14} /> {$t('call.log.save')}</button>
    </div>
    {#if savedPath}<p class="small muted">{$t('call.log.saved', { path: savedPath })}</p>{/if}

    {#if targets.length}
      <div class="row send">
        <select bind:value={chatId} aria-label={$t('call.log.chat')}>
          {#each targets as c (c.id)}<option value={c.id}>{c.name}</option>{/each}
        </select>
        <button class="primary with-icon" on:click={send} disabled={sending || !chatId}><Icon name="send" size={14} /> {$t('call.log.send')}</button>
      </div>
      {#if sentTo}<p class="small ok">{$t('call.log.sent', { name: sentTo })}</p>{/if}
    {/if}
  </div>
</div>

<style>
  .layer { position: fixed; inset: 0; z-index: 900; display: grid; place-items: center; padding: 24px; }
  .backdrop { position: absolute; inset: 0; background: var(--scrim); backdrop-filter: blur(6px); }
  .dialog {
    position: relative; width: min(680px, 100%); max-height: min(86vh, 900px); overflow: auto;
    display: flex; flex-direction: column; gap: 12px; padding: 20px;
    background: var(--panel); border: 1px solid var(--line-soft); border-radius: var(--radius-xl);
    box-shadow: var(--shadow-lg);
  }
  header { display: flex; gap: 12px; align-items: flex-start; }
  header p { margin-top: 4px; }
  .text {
    margin: 0; padding: 12px; overflow: auto;
    background: var(--raised); border-radius: 12px; font-size: 12px; line-height: 1.5; white-space: pre-wrap; word-break: break-word;
  }
  .btns { gap: 8px; flex-wrap: wrap; }
  .send { gap: 8px; }
  .send select { flex: 1; min-width: 0; }
  .ok { color: var(--color-success); }
</style>
