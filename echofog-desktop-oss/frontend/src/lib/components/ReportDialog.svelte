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
  import Button from './ui/Button.svelte';
  import Sheet from './ui/Sheet.svelte';
  import Badge from './ui/Badge.svelte';
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
</script>

<Sheet title={$t('report.title')} hint={$t('report.hint')} width={720} on:close={close}>
  {#if error}<p class="small error" role="alert">{error}</p>{/if}
  {#if report}
    <section class="what" aria-label={$t('report.willSend')}>
      <div class="col">
        <div class="eyebrow">{$t('report.willSend')}</div>
        <p class="small muted">{$t('report.included')}</p>
        {#if report.country || report.provider}
          <div class="row wrap tags">
            {#if report.country}<Badge variant="neutral">{report.country}</Badge>{/if}
            {#if report.provider}<Badge variant="neutral">{report.provider}</Badge>{/if}
          </div>
        {/if}
      </div>
      <div class="col">
        <div class="eyebrow">{$t('report.removedTitle')}</div>
        <p class="small muted"><Icon name="shield" size={14} /> {$t('report.removed')}</p>
      </div>
    </section>
    <!-- svelte-ignore a11y-no-noninteractive-tabindex -->
    <pre class="text mono" tabindex="0" aria-label={$t('report.title')}>{report.text}</pre>
    <p class="small hint"><Icon name="eye" size={14} /> {$t('report.check')}</p>
    {#if savedPath}<p class="small muted" role="status">{$t('report.saved', { path: savedPath })}</p>{/if}
    <p class="small faint">{$t('report.nothingSent')} {$t('report.issueHint')}</p>
  {:else if !error}
    <p class="small muted">{$t('report.building')}</p>
  {/if}
  <svelte:fragment slot="footer">
    {#if report}
      <Button icon={copied ? 'check' : 'copy'} on:click={copyReport}>{copied ? $t('common.copied') : $t('common.copy')}</Button>
      <Button icon="download" on:click={save}>{$t('common.save')}</Button>
      <span class="grow"></span>
      <Button variant="primary" icon="external" on:click={openIssue}>{$t('report.issue')}</Button>
    {:else}
      <span class="grow"></span>
      <Button on:click={close}>{$t('common.close')}</Button>
    {/if}
  </svelte:fragment>
</Sheet>

<style>
  .what { display: grid; grid-template-columns: 1fr 1fr; gap: var(--sp-4); padding: var(--sp-3); border-radius: var(--r-lg); background: var(--surface-2); }
  .what .col { display: flex; flex-direction: column; gap: var(--sp-1); min-width: 0; }
  .what p :global(svg) { vertical-align: -2px; color: var(--ok-fg); }
  .tags { gap: var(--sp-1); margin-top: var(--sp-1); }
  .text {
    margin: 0; padding: var(--sp-3); overflow: auto; min-height: 140px; max-height: 38vh;
    background: var(--surface-2); border: 1px solid var(--border); border-radius: var(--r-lg);
    font-size: 12px; line-height: 1.5; white-space: pre-wrap; word-break: break-word;
  }
  .hint { display: flex; gap: 6px; align-items: flex-start; color: var(--warn-fg); }
  .hint :global(svg) { flex: none; margin-top: 2px; }
  @media (max-width: 560px) { .what { grid-template-columns: 1fr; } }
</style>
