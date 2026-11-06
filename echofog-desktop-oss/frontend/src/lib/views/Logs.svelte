<script>
  import { onMount, onDestroy, tick } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import Button from '../components/ui/Button.svelte';
  import IconButton from '../components/ui/IconButton.svelte';
  import Input from '../components/ui/Input.svelte';
  import Tabs from '../components/ui/Tabs.svelte';
  import Toggle from '../components/ui/Toggle.svelte';
  import Icon from '../components/ui/Icon.svelte';
  import ReportDialog from '../components/ReportDialog.svelte';
  import { t, lang, intlLocale } from '../i18n/index.js';

  let entries = [];
  let level = 'all';        // all | debug | info | warn | error
  let query = '';
  let error = '';
  let copied = false;
  let savedPath = '';
  let autoRefresh = true;
  let timer = null;
  let list;
  let unseen = 0; // new lines that arrived while the list was scrolled away from them

  // newest at the top by default; the choice is remembered
  let newestFirst = true;
  try { newestFirst = localStorage.getItem('echofog.logsOrder') !== 'oldest'; } catch (_) {}
  function setOrder(v) {
    newestFirst = v;
    try { localStorage.setItem('echofog.logsOrder', v ? 'newest' : 'oldest'); } catch (_) {}
    unseen = 0;
    tick().then(jumpToNew);
  }

  const order = { debug: 0, info: 1, warn: 2, error: 3 };

  // the edge where new lines appear: the top for newest-first, the bottom otherwise
  const atNewEdge = () => !list || (newestFirst ? list.scrollTop < 24 : list.scrollHeight - list.scrollTop - list.clientHeight < 24);
  function jumpToNew() {
    if (list) list.scrollTop = newestFirst ? 0 : list.scrollHeight;
    unseen = 0;
  }

  async function load() {
    try {
      const next = (await App.GetLogs(2000)) || [];
      // keep what the reader is looking at in place when lines arrive above it
      const follow = atNewEdge();
      const before = list ? list.scrollHeight - list.scrollTop : 0;
      // the ring keeps the last 2000 lines, so count what is newer than the last line we had
      const lastT = entries.length ? Date.parse(entries[entries.length - 1].time) : 0;
      const added = entries.length ? next.filter((e) => Date.parse(e.time) > lastT).length : 0;
      entries = next;
      await tick();
      if (follow) jumpToNew();
      else if (list && newestFirst) list.scrollTop = list.scrollHeight - before;
      if (!follow && added) unseen += added;
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }

  function matches(e) {
    if (level !== 'all' && order[e.level] < order[level]) return false;
    if (query) {
      const q = query.toLowerCase();
      const hay = (e.message + ' ' + JSON.stringify(e.attrs || {})).toLowerCase();
      if (!hay.includes(q)) return false;
    }
    return true;
  }

  async function copyAll() {
    try {
      const text = await App.GetLogsText();
      await copy(text);
      copied = true;
      setTimeout(() => (copied = false), 1500);
    } catch (err) {
      error = errText(err);
    }
  }

  async function saveFile() {
    try {
      savedPath = await App.ExportLogs();
      setTimeout(() => (savedPath = ''), 6000);
    } catch (err) {
      error = errText(err);
    }
  }

  // hh:mm:ss.mmm, with the date when the line is not from today
  function fmtTime(t) {
    const d = new Date(t);
    if (isNaN(d)) return t;
    const time = d.toLocaleTimeString(intlLocale(), { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0');
    return d.toDateString() === new Date().toDateString() ? time : d.toLocaleDateString(intlLocale(), { day: '2-digit', month: '2-digit' }) + ' ' + time;
  }

  function attrLine(attrs) {
    if (!attrs) return '';
    return Object.entries(attrs).map(([k, v]) => `${k}=${v}`).join('  ');
  }

  // the core returns lines in time order, oldest first; equal times keep their order
  $: filtered = newestFirst ? entries.filter(matches).reverse() : entries.filter(matches);

  onMount(async () => {
    await load();
    jumpToNew();
    timer = setInterval(() => { if (autoRefresh) load(); }, 2000);
  });
  onDestroy(() => clearInterval(timer));
  let reporting = false;
</script>

<div class="logs">
  <header class="head">
    <div class="grow">
      <h1>{$t('logs.title')}</h1>
      <p class="muted small">{$t('logs.subtitle')}</p>
    </div>
    <Button variant="primary" size="sm" icon="flag" on:click={() => (reporting = true)}>{$t('report.open')}</Button>
  </header>

  <div class="bar">
    <Tabs variant="segment" label={$t('logs.filter')} bind:value={level}
      items={['all', 'debug', 'info', 'warn', 'error'].map((l) => ({ id: l, label: l === 'all' ? $t('logs.all') : l }))} />
    <div class="search"><Input size="sm" icon="search" aria-label={$t('logs.filter')} placeholder={$t('logs.filter')} bind:value={query} spellcheck="false" /></div>
    <Toggle bind:checked={autoRefresh} label={$t('logs.live')} />
    <div class="row btns">
      <IconButton icon="refresh" label={$t('common.refresh')} on:click={load} />
      <Button size="sm" icon={copied ? 'check' : 'copy'} on:click={copyAll}>{copied ? $t('common.copied') : $t('logs.copyAll')}</Button>
      <Button size="sm" icon="download" on:click={saveFile}>{$t('common.save')}</Button>
    </div>
  </div>

  <p class="privacy" role="note"><Icon name="eye" size={14} /> <span><b>{$t('logs.warnTitle')}</b> {$t('logs.warnText')}</span></p>

  {#if error}<div class="small error" role="alert">{error}</div>{/if}
  {#if savedPath}<div class="small ok" role="status">{$t('logs.savedTo', { path: savedPath })}</div>{/if}

  <div class="row small meta">
    <Tabs variant="segment" label={$t('logs.newest')} value={newestFirst ? 'new' : 'old'} on:change={(e) => setOrder(e.detail === 'new')}
      items={[{ id: 'new', label: $t('logs.newest') }, { id: 'old', label: $t('logs.oldest') }]} />
    <span class="faint grow">{$t('logs.shown', { n: filtered.length })}</span>
    {#if unseen}
      <Button size="sm" variant="primary" icon={newestFirst ? 'chevronUp' : 'chevronDown'} on:click={jumpToNew}>{$t('logs.jump')} · {unseen}</Button>
    {/if}
  </div>

  <div class="list" bind:this={list} on:scroll={() => { if (atNewEdge()) unseen = 0; }} role="log" aria-label={$t('logs.title')}>
    {#if filtered.length === 0}
      <div class="empty">{$t('logs.empty')}</div>
    {/if}
    {#each filtered as e}
      <div class="line {e.level}">
        <span class="time">{fmtTime(e.time, $lang)}</span>
        <span class="lvl">{e.level}</span>
        <span class="msg">{e.message}{#if e.attrs && Object.keys(e.attrs).length}<span class="attrs">  {attrLine(e.attrs)}</span>{/if}</span>
      </div>
    {/each}
  </div>
</div>

{#if reporting}<ReportDialog on:close={() => (reporting = false)} />{/if}

<style>
  .logs { display: flex; flex-direction: column; height: 100%; gap: var(--sp-3); padding: var(--sp-6) var(--sp-6) var(--sp-4); }
  .head { display: flex; align-items: flex-end; gap: var(--sp-3); }
  .bar { display: flex; flex-wrap: wrap; gap: var(--sp-3); align-items: center; }
  .search { flex: 1; min-width: 180px; }
  .btns { gap: var(--sp-1); }
  .privacy {
    display: flex; gap: var(--sp-2); align-items: flex-start; padding: 6px var(--sp-3); border-radius: var(--r-md);
    background: var(--surface-2); font-size: var(--fs-xs); line-height: var(--lh-xs); color: var(--muted);
  }
  .privacy :global(svg) { flex: none; margin-top: 1px; color: var(--faint); }
  .privacy b { color: var(--text); font-weight: var(--fw-medium); }
  .meta { gap: var(--sp-3); flex-wrap: wrap; }

  /* a dense monospace table: one line per record */
  .list {
    flex: 1; overflow: auto; font-family: var(--mono); font-size: 12px; line-height: 18px;
    background: var(--surface-1); border: 1px solid var(--border); border-radius: var(--r-xl); padding: var(--sp-1) 0;
  }
  .empty { color: var(--faint); padding: var(--sp-3); font-family: var(--font); font-size: var(--fs-sm); }
  .line { display: flex; gap: var(--sp-3); white-space: pre-wrap; word-break: break-word; padding: 1px var(--sp-3); border-left: 2px solid transparent; }
  .line:hover { background: var(--hover); }
  .time { color: var(--faint); flex-shrink: 0; font-variant-numeric: tabular-nums; }
  .lvl { flex-shrink: 0; width: 40px; text-transform: uppercase; font-size: 10.5px; font-weight: 600; letter-spacing: 0.04em; color: var(--faint); }
  .msg { flex: 1; color: var(--text); }
  .attrs { color: var(--faint); margin-left: var(--sp-2); }
  .line.info .lvl { color: var(--accent-fg); }
  .line.warn { border-left-color: var(--warn); }
  .line.warn .lvl, .line.warn .msg { color: var(--warn-fg); }
  .line.error { border-left-color: var(--danger); background: var(--danger-soft); }
  .line.error .lvl, .line.error .msg { color: var(--danger-fg); }
  .line.debug .msg { color: var(--muted); }
</style>
