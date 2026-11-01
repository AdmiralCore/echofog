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

  function portal(node) {
    document.body.appendChild(node);
    return { destroy: () => node.remove() };
  }
</script>

<svelte:window on:keydown={(e) => e.key === 'Escape' && close()} />

<div class="layer" use:portal>
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" on:click={close}></div>
  <div class="dialog" role="dialog" aria-label={$t('crash.title')}>
    <header>
      <div class="grow">
        <h2>{$t('crash.title')}</h2>
        <p class="small muted">{$t('crash.hint')}</p>
      </div>
      <button class="icon" title={$t('common.close')} aria-label={$t('common.close')} on:click={close}><Icon name="close" size={18} /></button>
    </header>

    <div class="reason">
      <b class="mono">{crash.reason}</b>
      {#if crash.where}<span class="mono small muted">{crash.where}</span>{/if}
    </div>
    <pre class="text mono">{text}</pre>
    <p class="small warn"><Icon name="eye" size={14} /> {$t('crash.check')}</p>
    {#if error}<p class="small error">{error}</p>{/if}

    <div class="row btns">
      <button class="with-icon" on:click={copyText}><Icon name={copied ? 'check' : 'copy'} size={14} /> {copied ? $t('common.copied') : $t('common.copy')}</button>
      <button class="with-icon" on:click={save}><Icon name="download" size={14} /> {$t('common.save')}</button>
      <button class="primary with-icon" on:click={openIssue}><Icon name="external" size={14} /> {$t('crash.issue')}</button>
    </div>
    {#if savedPath}<p class="small muted">{$t('crash.saved', { path: savedPath })}</p>{/if}

    {#if targets.length}
      <div class="row send">
        <select bind:value={chatId} aria-label={$t('crash.chat')}>
          {#each targets as c (c.id)}<option value={c.id}>{c.name}</option>{/each}
        </select>
        <button class="with-icon" on:click={send} disabled={sending || !chatId}><Icon name="send" size={14} /> {$t('crash.send')}</button>
      </div>
      {#if sentTo}<p class="small ok">{$t('crash.sent', { name: sentTo })}</p>{/if}
    {/if}
  </div>
</div>

<style>
  .layer { position: fixed; inset: 0; z-index: 900; display: grid; place-items: center; padding: 24px; }
  .backdrop { position: absolute; inset: 0; background: var(--scrim); backdrop-filter: blur(6px); }
  .dialog {
    position: relative; width: min(760px, 100%); max-height: min(86vh, 900px); overflow: auto;
    display: flex; flex-direction: column; gap: 12px; padding: 20px;
    background: var(--panel); border: 1px solid var(--line-soft); border-radius: var(--radius-xl);
    box-shadow: var(--shadow-lg);
  }
  header { display: flex; gap: 12px; align-items: flex-start; }
  header p { margin-top: 4px; }
  .reason { display: flex; flex-direction: column; gap: 4px; word-break: break-word; }
  .text {
    margin: 0; padding: 12px; overflow: auto; min-height: 120px; max-height: 40vh;
    background: var(--raised); border-radius: 12px; font-size: 12px; line-height: 1.5; white-space: pre-wrap; word-break: break-word;
  }
  .btns { gap: 8px; flex-wrap: wrap; }
  .send { gap: 8px; }
  .send select { flex: 1; min-width: 0; }
  .ok { color: var(--color-success); }
  .warn { color: #c98a00; display: flex; gap: 6px; align-items: flex-start; }
</style>
