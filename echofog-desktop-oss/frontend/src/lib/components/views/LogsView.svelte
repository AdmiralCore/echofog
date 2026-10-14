<script>
  import { onMount, onDestroy } from 'svelte';
  import { GetLogs, GetLogsText, ExportLogs } from '@wailsjs/go/main/App';

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
      entries = (await GetLogs(2000)) || [];
      error = '';
    } catch (err) {
      error = (err && (err.message || String(err))) || 'Could not read logs';
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
      const text = await GetLogsText();
      await navigator.clipboard.writeText(text);
      copied = true;
      setTimeout(() => (copied = false), 1500);
    } catch (err) {
      error = (err && (err.message || String(err))) || 'Copy failed';
    }
  }

  async function saveFile() {
    try {
      savedPath = await ExportLogs();
      setTimeout(() => (savedPath = ''), 6000);
    } catch (err) {
      error = (err && (err.message || String(err))) || 'Save failed';
    }
  }

  const fmtTime = (t) => { try { return new Date(t).toLocaleTimeString(); } catch (_) { return t; } };
  const attrLine = (attrs) => attrs ? Object.entries(attrs).map(([k, v]) => `${k}=${v}`).join('  ') : '';

  $: filtered = entries.filter(matches);

  onMount(() => {
    load();
    timer = setInterval(() => { if (autoRefresh) load(); }, 2000);
  });
  onDestroy(() => clearInterval(timer));
</script>

<div class="logs">
  <header>
    <h2>Logs</h2>
    <p class="hint">Everything the app and the node record — network mode, info, warnings and errors.
      Sensitive values are hidden, so this is safe to share in a bug report.</p>
  </header>

  <div class="bar">
    <div class="levels">
      {#each ['all', 'debug', 'info', 'warn', 'error'] as l}
        <button class:active={level === l} on:click={() => (level = l)}>{l}</button>
      {/each}
    </div>
    <input class="search" placeholder="filter…" bind:value={query} />
    <label class="auto"><input type="checkbox" bind:checked={autoRefresh} /> live</label>
    <button on:click={load}>Refresh</button>
    <button on:click={copyAll}>{copied ? 'Copied' : 'Copy all'}</button>
    <button on:click={saveFile}>Save to file</button>
  </div>

  {#if error}<div class="err">{error}</div>{/if}
  {#if savedPath}<div class="ok">Saved to {savedPath}</div>{/if}

  <div class="list">
    {#if filtered.length === 0}<div class="empty">No log lines yet.</div>{/if}
    {#each filtered as e}
      <div class="row {e.level}">
        <span class="time">{fmtTime(e.time)}</span>
        <span class="lvl">{e.level}</span>
        <span class="msg">{e.message}{#if e.attrs && Object.keys(e.attrs).length}<span class="attrs">  {attrLine(e.attrs)}</span>{/if}</span>
      </div>
    {/each}
  </div>
</div>

<style>
  .logs { display: flex; flex-direction: column; height: 100%; gap: 10px; padding: 16px; box-sizing: border-box; }
  header h2 { margin: 0 0 2px; font-size: 18px; font-weight: 700; }
  .hint { margin: 0; font-size: 12px; opacity: 0.6; max-width: 72ch; }
  .bar { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
  .levels { display: flex; gap: 2px; }
  .levels button, .bar > button { background: rgba(127,127,127,0.14); border: none; color: inherit;
    padding: 5px 10px; border-radius: 6px; cursor: pointer; font-size: 12px; }
  .levels button.active { background: rgba(127,127,127,0.32); font-weight: 600; }
  .search { flex: 1; min-width: 120px; padding: 5px 10px; border-radius: 6px;
    border: 1px solid rgba(127,127,127,0.25); background: transparent; color: inherit; }
  .auto { font-size: 12px; opacity: 0.7; display: flex; align-items: center; gap: 4px; }
  .err { color: #e5534b; font-size: 13px; }
  .ok { color: #3fb950; font-size: 13px; }
  .list { flex: 1; overflow: auto; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 12px; line-height: 1.5; background: rgba(0,0,0,0.2); border-radius: 8px; padding: 8px; }
  .empty { opacity: 0.5; padding: 12px; }
  .row { display: flex; gap: 8px; white-space: pre-wrap; word-break: break-word; padding: 1px 4px; }
  .time { opacity: 0.45; flex-shrink: 0; }
  .lvl { flex-shrink: 0; width: 44px; text-transform: uppercase; font-size: 10px; align-self: center; opacity: 0.8; }
  .msg { flex: 1; }
  .attrs { opacity: 0.55; }
  .row.warn .lvl, .row.warn .msg { color: #d29922; }
  .row.error .lvl, .row.error .msg { color: #e5534b; }
  .row.debug { opacity: 0.6; }
</style>
