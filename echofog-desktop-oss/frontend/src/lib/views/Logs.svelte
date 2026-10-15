<script>
  import { onMount, onDestroy } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import Icon from '../components/ui/Icon.svelte';

  let entries = [];
  let level = 'all';        // all | debug | info | warn | error
  let query = '';
  let error = '';
  let copied = false;
  let savedPath = '';
  let autoRefresh = true;
  let timer = null;

  const order = { debug: 0, info: 1, warn: 2, error: 3 };

  async function load() {
    try {
      entries = (await App.GetLogs(2000)) || [];
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

  function fmtTime(t) {
    try { return new Date(t).toLocaleTimeString(); } catch (_) { return t; }
  }

  function attrLine(attrs) {
    if (!attrs) return '';
    return Object.entries(attrs).map(([k, v]) => `${k}=${v}`).join('  ');
  }

  $: filtered = entries.filter(matches);

  onMount(() => {
    load();
    timer = setInterval(() => { if (autoRefresh) load(); }, 2000);
  });
  onDestroy(() => clearInterval(timer));
</script>

<div class="logs">
  <div class="page-head">
    <div class="grow">
      <h1>Logs</h1>
      <p class="muted small">Everything the app and the node record. Sensitive values are hidden, so this is safe to share in a bug report.</p>
    </div>
  </div>

  <div class="bar">
    <div class="seg levels">
      {#each ['all', 'debug', 'info', 'warn', 'error'] as l}
        <button class:active={level === l} on:click={() => (level = l)}>{l}</button>
      {/each}
    </div>
    <div class="search">
      <Icon name="search" size={15} />
      <input placeholder="Filter" bind:value={query} spellcheck="false" />
    </div>
    <label class="row live"><input type="checkbox" class="switch" bind:checked={autoRefresh} /> Live</label>
    <div class="row btns">
      <button class="icon" title="Refresh" aria-label="Refresh" on:click={load}><Icon name="refresh" size={17} /></button>
      <button class="with-icon small" on:click={copyAll}><Icon name={copied ? 'check' : 'copy'} size={14} />{copied ? 'Copied' : 'Copy all'}</button>
      <button class="with-icon small" on:click={saveFile}><Icon name="download" size={14} /> Save</button>
    </div>
  </div>

  {#if error}<div class="small error">{error}</div>{/if}
  {#if savedPath}<div class="small ok">Saved to {savedPath}</div>{/if}

  <div class="list">
    {#if filtered.length === 0}
      <div class="empty">No log lines yet.</div>
    {/if}
    {#each filtered as e}
      <div class="line {e.level}">
        <span class="time">{fmtTime(e.time)}</span>
        <span class="lvl">{e.level}</span>
        <span class="msg">{e.message}{#if e.attrs && Object.keys(e.attrs).length}<span class="attrs">  {attrLine(e.attrs)}</span>{/if}</span>
      </div>
    {/each}
  </div>
</div>

<style>
  .logs { display: flex; flex-direction: column; height: 100%; gap: 12px; padding: clamp(18px, 4vw, 36px) clamp(16px, 4vw, 40px) 20px; animation: fade-up 0.35s var(--ease); }
  .bar { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; }
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
  .attrs { color: var(--faint); }
  .line.info .lvl { color: var(--accent); background: var(--accent-soft); }
  .line.warn .lvl { color: var(--warn); background: var(--warn-soft); }
  .line.warn .msg { color: var(--warn); }
  .line.error .lvl { color: var(--danger); background: var(--danger-soft); }
  .line.error .msg { color: var(--danger); }
  .line.debug { opacity: 0.65; }
</style>
