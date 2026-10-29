<script>
  /**
   * The anonymous connection report: what the node knows about how it connects (country, provider,
   * NAT, transports, guards, VPN, the latest problems), with no address, ID, link or contact in it.
   * Nothing is sent: the user reads it, then copies it, saves it, or opens the report form on
   * GitHub with it filled in, and posts it there themselves.
   */
  import { onMount, createEventDispatcher } from 'svelte';
  import { App, errText, copy, openURL } from '../api.js';
  import Icon from './ui/Icon.svelte';
  import { t } from '../i18n/index.js';

  const dispatch = createEventDispatcher();
  const ISSUE = 'https://github.com/AdmiralCore/echofog/issues/new';

  let report = null;
  let error = '';
  let copied = false;
  let savedPath = '';

  onMount(async () => {
    try { report = await App.ConnectionReport(); } catch (err) { error = errText(err); }
  });

  const close = () => dispatch('close');

  async function copyReport() {
    await copy(report.text);
    copied = true;
    setTimeout(() => (copied = false), 1500);
  }

  async function save() {
    try { savedPath = await App.SaveConnectionReport(report.text); } catch (err) { error = errText(err); }
  }

  // the report form, filled in: the fields of .github/ISSUE_TEMPLATE/censorship_report.yml by id
  async function openIssue() {
    const q = new URLSearchParams({ template: 'censorship_report.yml', version: report.version || '', log: report.text });
    if (report.country) q.set('country', report.country);
    if (report.provider) q.set('provider', report.provider);
    let url = ISSUE + '?' + q.toString();
    if (url.length > 7500) {
      // GitHub cuts long addresses: the form opens without the report, which goes to the clipboard
      q.delete('log');
      url = ISSUE + '?' + q.toString();
      await copyReport();
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
  <div class="dialog" role="dialog" aria-label={$t('report.title')}>
    <header>
      <div class="grow">
        <h2>{$t('report.title')}</h2>
        <p class="small muted">{$t('report.hint')}</p>
      </div>
      <button class="icon" title={$t('common.close')} aria-label={$t('common.close')} on:click={close}><Icon name="close" size={18} /></button>
    </header>

    {#if error}<p class="small error">{error}</p>{/if}
    {#if report}
      <pre class="text mono">{report.text}</pre>
      <p class="small warn"><Icon name="eye" size={14} /> {$t('report.check')}</p>
      <div class="row btns">
        <button class="with-icon" on:click={copyReport}><Icon name={copied ? 'check' : 'copy'} size={14} /> {copied ? $t('common.copied') : $t('common.copy')}</button>
        <button class="with-icon" on:click={save}><Icon name="download" size={14} /> {$t('common.save')}</button>
        <button class="primary with-icon" on:click={openIssue}><Icon name="external" size={14} /> {$t('report.issue')}</button>
      </div>
      {#if savedPath}<p class="small muted">{$t('report.saved', { path: savedPath })}</p>{/if}
      <p class="small faint">{$t('report.issueHint')}</p>
    {:else if !error}
      <p class="small muted">{$t('report.building')}</p>
    {/if}
  </div>
</div>

<style>
  .layer { position: fixed; inset: 0; z-index: 900; display: grid; place-items: center; padding: 24px; }
  .backdrop { position: absolute; inset: 0; background: var(--scrim); backdrop-filter: blur(6px); }
  .dialog {
    position: relative; width: min(760px, 100%); max-height: min(86vh, 900px);
    display: flex; flex-direction: column; gap: 12px; padding: 20px;
    background: var(--panel); border: 1px solid var(--line-soft); border-radius: var(--radius-xl);
    box-shadow: var(--shadow-lg);
  }
  header { display: flex; gap: 12px; align-items: flex-start; }
  header p { margin-top: 4px; }
  .text {
    margin: 0; padding: 12px; overflow: auto; min-height: 160px; max-height: 46vh;
    background: var(--raised); border-radius: 12px; font-size: 12px; line-height: 1.5; white-space: pre-wrap; word-break: break-word;
  }
  .btns { gap: 8px; flex-wrap: wrap; }
  .warn { color: #c98a00; display: flex; gap: 6px; align-items: flex-start; }
</style>
