<script>
  import { onMount, onDestroy, tick } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import Icon from '../components/ui/Icon.svelte';
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
</script>

<div class="logs">
  <div class="page-head">
    <div class="grow">
      <h1>{$t('logs.title')}</h1>
      <p class="muted small">{$t('logs.subtitle')}</p>
    </div>
  </div>

  <div class="bar">
    <div class="seg levels">
      {#each ['all', 'debug', 'info', 'warn', 'error'] as l}
        <button class:active={level === l} on:click={() => (level = l)}>{l === 'all' ? $t('logs.all') : l}</button>
      {/each}
    </div>
    <div class="search">
      <Icon name="search" size={15} />
      <input placeholder={$t('logs.filter')} bind:value={query} spellcheck="false" />
    </div>
    <label class="row live"><input type="checkbox" class="switch" bind:checked={autoRefresh} /> {$t('logs.live')}</label>
    <div class="row btns">
      <button class="icon" title={$t('common.refresh')} aria-label={$t('common.refresh')} on:click={load}><Icon name="refresh" size={17} /></button>
      <button class="with-icon small" on:click={copyAll}><Icon name={copied ? 'check' : 'copy'} size={14} />{copied ? $t('common.copied') : $t('logs.copyAll')}</button>
      <button class="with-icon small" on:click={saveFile}><Icon name="download" size={14} /> {$t('common.save')}</button>
    </div>
  </div>

  <div class="warn-box" role="note">
    <span class="w-ic"><Icon name="eye" size={16} /></span>
    <div><b>{$t('logs.warnTitle')}</b> {$t('logs.warnText')}</div>
  </div>

  {#if error}<div class="small error">{error}</div>{/if}
  {#if savedPath}<div class="small ok">{$t('logs.savedTo', { path: savedPath })}</div>{/if}

  <div class="row small meta">
    <div class="seg order">
      <button class:active={newestFirst} on:click={() => setOrder(true)}>{$t('logs.newest')}</button>
      <button class:active={!newestFirst} on:click={() => setOrder(false)}>{$t('logs.oldest')}</button>
    </div>
    <span class="faint grow">{$t('logs.shown', { n: filtered.length })}</span>
    {#if unseen}
      <button class="small primary jump" on:click={jumpToNew}><Icon name={newestFirst ? 'chevronUp' : 'chevronDown'} size={14} /> {$t('logs.jump')} · {unseen}</button>
    {/if}
  </div>

  <div class="list" bind:this={list} on:scroll={() => { if (atNewEdge()) unseen = 0; }}>
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

<style>
  .logs { display: flex; flex-direction: column; height: 100%; gap: 12px; padding: clamp(18px, 4vw, 36px) clamp(16px, 4vw, 40px) 20px; animation: fade-up 0.35s var(--ease); }
  .bar { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; }
  .warn-box {
    display: flex; gap: 12px; align-items: flex-start; padding: 10px 14px; border-radius: var(--radius);
    background: var(--warn-soft); border-left: 3px solid var(--warn); font-size: 12.5px; color: var(--muted); line-height: 1.45;
  }
  .warn-box b { color: var(--text); }
  .w-ic { color: var(--warn); flex: none; margin-top: 1px; }
  .meta { gap: 10px; flex-wrap: wrap; }
  .order button { padding: 4px 12px; font-size: 12px; }
  .jump { display: inline-flex; align-items: center; gap: 6px; animation: pop 0.3s var(--spring); }
  .levels button { padding: 5px 12px; font-size: 12.5px; text-transform: capitalize; }
  .search {
    flex: 1; min-width: 160px; display: flex; align-items: center; gap: 8px; height: 36px; padding: 0 12px;
    border-radius: 12px; background: var(--raised); color: var(--faint); border: 1px solid transparent;
    transition: border-color var(--dur), box-shadow var(--dur), color var(--dur);
  }
  .search:focus-within { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); color: var(--accent); }
  .search input { border: none; background: transparent; padding: 0; height: 100%; }
  .search input:focus { box-shadow: none; background: transparent; }
  .live { font-size: 13px; gap: 8px; }
  .btns { gap: 4px; }
  .list {
    flex: 1; overflow: auto; font-family: var(--mono); font-size: 12px; line-height: 1.55;
    background: var(--panel); border: 1px solid var(--line-soft); border-radius: var(--radius-lg); padding: 10px 6px;
    box-shadow: var(--shadow-sm);
  }
  .empty { color: var(--faint); padding: 12px; }
  .line { display: flex; gap: 10px; white-space: pre-wrap; word-break: break-word; padding: 2px 8px; border-radius: 6px; }
  .line:hover { background: var(--hover); }
  .time { color: var(--faint); flex-shrink: 0; font-variant-numeric: tabular-nums; }
  .lvl {
    flex-shrink: 0; width: 50px; text-align: center; text-transform: uppercase; font-size: 9.5px; font-weight: 700; align-self: center;
    padding: 1px 0; border-radius: 5px; background: var(--hover); color: var(--muted); letter-spacing: 0.04em;
  }
  .msg { flex: 1; color: var(--text); }
  .attrs { color: var(--faint); margin-left: 10px; }
  .line.info .lvl { color: var(--accent); background: var(--accent-soft); }
  .line.warn .lvl { color: var(--warn); background: var(--warn-soft); }
  .line.warn .msg { color: var(--warn); }
  .line.error .lvl { color: var(--danger); background: var(--danger-soft); }
  .line.error .msg { color: var(--danger); }
  .line.debug { opacity: 0.65; }
</style>
