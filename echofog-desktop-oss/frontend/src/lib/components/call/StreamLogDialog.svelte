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
  import Button from '../ui/Button.svelte';
  import Sheet from '../ui/Sheet.svelte';
  import Select from '../ui/Select.svelte';
  import Badge from '../ui/Badge.svelte';
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
  $: options = targets.map((c) => ({ value: c.id, label: c.name }));
</script>

<Sheet title={$t('call.log.title')} hint={$t('call.log.hint')} width={680} on:close={close}>
  <!-- svelte-ignore a11y-no-noninteractive-tabindex -->
  <pre class="text mono" tabindex="0" aria-label={$t('call.log.title')}>{text}</pre>
  <p class="small faint">{$t('call.log.inside', { s: log.summary.seconds, n: log.lines })}</p>
  {#if error}<p class="small error" role="alert">{error}</p>{/if}
  {#if savedPath}<p class="small muted" role="status">{$t('call.log.saved', { path: savedPath })}</p>{/if}
  {#if sentTo}<p class="small ok-text" role="status"><Badge variant="ok" icon="check">{$t('call.log.sent', { name: sentTo })}</Badge></p>{/if}

  <svelte:fragment slot="footer">
    <Button icon={copied ? 'check' : 'copy'} on:click={copySummary}>{copied ? $t('common.copied') : $t('call.log.copy')}</Button>
    <Button icon="download" on:click={save}>{$t('call.log.save')}</Button>
    <span class="grow"></span>
    {#if targets.length}
      <div class="sendrow">
        <div class="pick"><Select size="sm" bind:value={chatId} {options} aria-label={$t('call.log.chat')} /></div>
        <Button variant="primary" icon="send" loading={sending} disabled={!chatId} on:click={send}>{$t('call.log.send')}</Button>
      </div>
    {/if}
  </svelte:fragment>
</Sheet>

<style>
  .text {
    margin: 0; padding: var(--sp-3); overflow: auto; min-height: 120px; max-height: 40vh;
    background: var(--surface-2); border: 1px solid var(--border); border-radius: var(--r-lg);
    font-size: 12px; line-height: 1.5; white-space: pre-wrap; word-break: break-word;
  }
  .sendrow { display: flex; align-items: center; gap: var(--sp-2); flex: 0 1 auto; }
  .pick { min-width: 140px; max-width: 220px; }
</style>
