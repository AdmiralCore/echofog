<script>
  /** A file in a chat: what it is, how far it got, and what can be done with it. */
  import { transfers, accept, decline, cancel, openFile, showInFolder, progress, eta } from '../stores/files.js';
  import { bytes, duration } from '../format.js';
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  export let file; // { fid, name, size } from the message
  export let mine = false;

  $: xfer = $transfers[file.fid];
  // a file we posted in a group stays offered to the members who have not taken it; it shows
  // progress only while somebody downloads it, and how many have it
  $: shared = mine && !!xfer?.group;
  $: idle = shared && xfer.state === 'active' && !xfer.fetching;
  $: state = xfer ? (idle ? 'offered' : xfer.state) : 'unknown';
  $: size = xfer ? xfer.size : file.size;
  $: pct = Math.round(progress(xfer) * 100);
  $: left = eta(xfer);
  $: ext = (file.name.match(/\.([^.]{1,5})$/) || [])[1] || '';
  let busy = false;

  async function run(fn) {
    busy = true;
    try { await fn(); } finally { busy = false; }
  }
</script>

<div class="file" class:mine>
  <div class="top">
    <span class="ic" class:done={state === 'done'}>
      {#if state === 'active'}<Icon name={mine ? 'upload' : 'download'} size={18} />{:else}<Icon name="file" size={18} />{/if}
      {#if ext && state !== 'active'}<span class="ext">{ext.toUpperCase()}</span>{/if}
    </span>
    <div class="what">
      <span class="name" title={file.name}>{file.name}</span>
      <span class="sub">
        {#if state === 'active'}
          {bytes(xfer.done)} / {bytes(size)}{#if xfer.rate} · {$t('files.rate', { v: bytes(xfer.rate) })}{/if}{#if left} · {$t('files.left', { time: duration(left) })}{/if}
        {:else}
          {bytes(size)} · {#if state === 'unknown'}{$t('files.st.unknown')}{:else if shared && state === 'offered'}{$t('files.st.shared')}{:else if state === 'offered'}{mine ? $t('files.st.waitingAccept') : $t('files.st.offered')}{:else}{$t('files.st.' + state)}{/if}
        {/if}
      </span>
      {#if shared && xfer.members}
        <span class="sub delivered">
          <Icon name="check" size={12} /> {$t('files.delivered', { n: xfer.delivered || 0, of: xfer.members })}{#if xfer.fetching} · {$t('files.fetching', { n: xfer.fetching })}{/if}
        </span>
      {/if}
    </div>
  </div>

  {#if state === 'active'}
    <div class="bar" role="progressbar" aria-valuenow={pct} aria-valuemin="0" aria-valuemax="100" aria-label={$t('files.progress')}>
      <span style="width:{pct}%" class:stalled={xfer.stalled}></span>
    </div>
    {#if xfer.stalled}<span class="note">{$t('files.stalled')}</span>{/if}
  {/if}
  {#if xfer?.error && (state === 'failed')}<span class="note err">{xfer.error}</span>{/if}

  <div class="acts">
    {#if state === 'offered' && !mine}
      <button class="primary small with-icon" disabled={busy} on:click={() => run(() => accept(file.fid))}><Icon name="download" size={14} /> {$t('files.accept')}</button>
      <button class="ghost small" disabled={busy} on:click={() => run(() => decline(file.fid))}>{$t('files.decline')}</button>
    {:else if shared && (state === 'active' || state === 'offered')}
      <button class="ghost small" disabled={busy} title={$t('files.stopSharingHint')} on:click={() => run(() => cancel(file.fid))}>{$t('files.stopSharing')}</button>
      {#if xfer.path}<button class="ghost small with-icon" on:click={() => showInFolder(file.fid)}><Icon name="folder" size={14} /> {$t('files.showInFolder')}</button>{/if}
    {:else if state === 'active' || (state === 'offered' && mine)}
      <button class="ghost small" disabled={busy} on:click={() => run(() => cancel(file.fid))}>{$t('common.cancel')}</button>
    {:else if state === 'done' && !mine}
      <button class="small with-icon" on:click={() => openFile(xfer)}><Icon name="external" size={14} /> {$t('files.open')}</button>
      <button class="ghost small with-icon" on:click={() => showInFolder(file.fid)}><Icon name="folder" size={14} /> {$t('files.showInFolder')}</button>
    {:else if mine && xfer && xfer.path && state !== 'failed'}
      <button class="ghost small with-icon" on:click={() => showInFolder(file.fid)}><Icon name="folder" size={14} /> {$t('files.showInFolder')}</button>
    {/if}
  </div>
</div>

<style>
  .file { display: flex; flex-direction: column; gap: 8px; min-width: min(280px, 60vw); max-width: 360px; padding: 2px 0 2px; }
  .top { display: flex; gap: 10px; align-items: center; }
  .ic {
    position: relative; flex: none; width: 42px; height: 42px; border-radius: 12px; display: grid; place-items: center;
    background: var(--accent-soft); color: var(--accent);
  }
  .ic.done { background: var(--ok-soft); color: var(--ok); }
  .mine .ic, .mine .ic.done { background: rgba(255, 255, 255, 0.18); color: #fff; }
  .ext { position: absolute; bottom: -4px; right: -6px; padding: 1px 4px; border-radius: 5px; font-size: 8.5px; font-weight: 750; letter-spacing: 0.02em; background: var(--panel); color: var(--muted); border: 1px solid var(--line-soft); }
  .what { min-width: 0; display: flex; flex-direction: column; gap: 2px; }
  .name { font-weight: 620; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sub { font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
  .delivered { display: inline-flex; align-items: center; gap: 4px; }
  .mine .sub { color: rgba(255, 255, 255, 0.78); }
  .bar { height: 5px; border-radius: 99px; background: var(--line); overflow: hidden; }
  .mine .bar { background: rgba(255, 255, 255, 0.25); }
  .bar span { display: block; height: 100%; border-radius: inherit; background: var(--accent); transition: width 0.25s linear; }
  .mine .bar span { background: #fff; }
  .bar span.stalled { opacity: 0.5; }
  .note { font-size: 11.5px; color: var(--muted); }
  .mine .note { color: rgba(255, 255, 255, 0.8); }
  .note.err { color: var(--danger); }
  .mine .note.err { color: #fff; font-weight: 600; }
  .acts { display: flex; gap: 6px; flex-wrap: wrap; }
  .acts:empty { display: none; }
  .mine .acts button { color: #fff; border-color: rgba(255, 255, 255, 0.4); background: rgba(255, 255, 255, 0.12); }
  .mine .acts button:hover:not(:disabled) { background: rgba(255, 255, 255, 0.22); }
</style>
