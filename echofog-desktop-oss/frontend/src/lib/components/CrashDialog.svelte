<script>
  /**
   * The crash of an earlier run: its reason and trace, and ways to hand it on — copy it, save it as
   * a file, send the file into a chat, or open the bug report form on GitHub with it filled in.
   * Nothing leaves the device unless the user sends it.
   */
  import { createEventDispatcher } from 'svelte';
  import { App, errText, copy, openURL } from '../api.js';
  import { chats } from '../stores/chats.js';
  import { sendPaths } from '../stores/files.js';
  import Icon from './ui/Icon.svelte';
  import Button from './ui/Button.svelte';
  import Select from './ui/Select.svelte';
  import Sheet from './ui/Sheet.svelte';
  import { t } from '../i18n/index.js';

  export let crash;

  const dispatch = createEventDispatcher();
  const close = () => dispatch('close');
  const ISSUE = 'https://github.com/AdmiralCore/echofog/issues/new';

  const text = `EchoFog v${crash.version} · ${crash.os}\n\n${crash.text}`;
  let error = '';
  let copied = false;
  let savedPath = '';
  let sending = false;
  let sentTo = '';

  $: targets = $chats.filter((c) => !c.blocked && !c.request && c.can?.files !== false && c.can?.send !== false);
  let chatId = '';
  $: if (!chatId && targets.length) chatId = targets[0].id;

  async function copyText() {
    await copy(text);
    copied = true;
    setTimeout(() => (copied = false), 1500);
  }

  async function save() {
    error = '';
    try { savedPath = await App.SaveCrashReport(text); } catch (err) { error = errText(err); }
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

  // the bug report form, filled in: the fields of .github/ISSUE_TEMPLATE/bug_report.yml by id
  async function openIssue() {
    const q = new URLSearchParams({
      template: 'bug_report.yml', title: crash.reason, version: 'v' + crash.version, os: crash.os,
      what: $t('crash.issueWhat'), log: text,
    });
    let url = ISSUE + '?' + q.toString();
    if (url.length > 7500) {
      // GitHub cuts long addresses: the form opens without the trace, which goes to the clipboard
      q.delete('log');
      url = ISSUE + '?' + q.toString();
      await copyText();
    }
    openURL(url);
  }
</script>

<Sheet title={$t('crash.title')} hint={$t('crash.hint')} width={720} on:close={close}>
  <div class="reason">
    <b class="mono">{crash.reason}</b>
    {#if crash.where}<span class="mono small muted">{crash.where}</span>{/if}
  </div>
  <!-- svelte-ignore a11y-no-noninteractive-tabindex -->
  <pre class="text mono" tabindex="0" aria-label={$t('crash.title')}>{text}</pre>
  <p class="small hint"><Icon name="eye" size={14} /> {$t('crash.check')}</p>
  {#if error}<p class="small error" role="alert">{error}</p>{/if}
  {#if savedPath}<p class="small muted" role="status">{$t('crash.saved', { path: savedPath })}</p>{/if}

  {#if targets.length}
    <div class="row send end">
      <div class="grow"><Select label={$t('crash.chat')} bind:value={chatId} options={targets.map((c) => ({ value: c.id, label: c.name }))} /></div>
      <Button icon="send" on:click={send} disabled={sending || !chatId}>{$t('crash.send')}</Button>
    </div>
    {#if sentTo}<p class="small ok" role="status">{$t('crash.sent', { name: sentTo })}</p>{/if}
  {/if}

  <svelte:fragment slot="footer">
    <Button icon={copied ? 'check' : 'copy'} on:click={copyText}>{copied ? $t('common.copied') : $t('common.copy')}</Button>
    <Button icon="download" on:click={save}>{$t('common.save')}</Button>
    <span class="grow"></span>
    <Button variant="primary" icon="external" on:click={openIssue}>{$t('crash.issue')}</Button>
  </svelte:fragment>
</Sheet>

<style>
  .reason { display: flex; flex-direction: column; gap: 4px; word-break: break-word; }
  .text {
    margin: 0; padding: var(--sp-3); overflow: auto; min-height: 120px; max-height: 36vh;
    background: var(--surface-2); border: 1px solid var(--border); border-radius: var(--r-lg);
    font-size: 12px; line-height: 1.5; white-space: pre-wrap; word-break: break-word;
  }
  .hint { display: flex; gap: 6px; align-items: flex-start; color: var(--warn-fg); }
  .hint :global(svg) { flex: none; margin-top: 2px; }
  .send { gap: var(--sp-2); align-items: flex-end; }
</style>
