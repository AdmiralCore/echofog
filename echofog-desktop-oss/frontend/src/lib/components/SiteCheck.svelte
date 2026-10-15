<script>
  /**
   * Which sites open directly and which through the proxy. Selected tiles are checked, and they are
   * also the sites the app uses to rank exits for the automatic choice.
   */
  import { onMount, createEventDispatcher } from 'svelte';
  import { App, errText } from '../api.js';
  import Icon from './ui/Icon.svelte';
  import { PRESET_SITES, MAX_SELECTED, siteTarget, hostOf, loadCustom, saveCustom } from '../sites.js';
  import { t } from '../i18n/index.js';

  export let exit = 'auto'; // peer id, 'auto', or '' for the direct check only
  export let exitLabel = ''; // defaults to "the automatic exit"

  const dispatch = createEventDispatcher();

  let custom = loadCustom();
  let selected = new Set();
  let results = {}; // target -> { direct, exit }
  let checking = false;
  let error = '';
  let adding = '';
  let addError = '';
  let saveTimer;

  $: tiles = [
    ...PRESET_SITES.map((p) => ({ ...p, target: siteTarget(p.host), preset: true })),
    ...custom.map((c) => ({ name: c.name || hostOf(siteTarget(c.host)), host: c.host, target: siteTarget(c.host), mark: (c.name || c.host).replace(/^www\./, '').slice(0, 2), color: colorFor(c.host), preset: false })),
  ].filter((t) => t.target);
  $: done = Object.keys(results).length;
  $: openDirect = tiles.filter((t) => results[t.target]?.direct?.ok).length;
  $: openExit = tiles.filter((t) => results[t.target]?.exit?.ok).length;

  function colorFor(s) {
    let h = 0;
    for (const ch of s) h = (h * 31 + ch.charCodeAt(0)) % 360;
    return `hsl(${h} 55% 55%)`;
  }

  const TOUCHED = 'echofog.sites.touched';
  const touched = () => { try { return !!localStorage.getItem(TOUCHED); } catch (_) { return false; } };
  const markTouched = () => { try { localStorage.setItem(TOUCHED, '1'); } catch (_) { /* private mode */ } };
  // what a new user checks: the services most often blocked
  const starter = () => new Set(PRESET_SITES.filter((p) => ['YouTube', 'Discord', 'Instagram', 'X', 'WhatsApp', 'Telegram'].includes(p.name)).map((p) => siteTarget(p.host)));

  onMount(async () => {
    if (!touched()) {
      selected = starter();
      markTouched();
      persistSelection();
      return;
    }
    try {
      const domains = (await App.GetProbeDomains()) || [];
      const known = new Set(PRESET_SITES.map((p) => siteTarget(p.host)));
      // sites the core checks that are not tiles yet become custom tiles
      const extra = domains.filter((d) => !known.has(d) && !custom.some((c) => siteTarget(c.host) === d));
      if (extra.length) {
        custom = [...custom, ...extra.map((d) => ({ host: hostOf(d) }))];
        saveCustom(custom);
      }
      selected = new Set(domains);
      if (!selected.size) selected = starter();
    } catch (_) {
      selected = starter();
    }
  });

  function persistSelection() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(async () => {
      try { await App.SetProbeDomains([...selected]); } catch (err) { error = errText(err); }
    }, 400);
  }

  function toggle(tile) {
    error = '';
    if (selected.has(tile.target)) selected.delete(tile.target);
    else if (selected.size >= MAX_SELECTED) { error = $t('proxy.sites.tooMany', { n: MAX_SELECTED }); return; }
    else selected.add(tile.target);
    selected = selected;
    persistSelection();
  }

  function add() {
    addError = '';
    const target = siteTarget(adding);
    if (!target) { addError = $t('proxy.sites.typeSite'); return; }
    if (tiles.some((t) => t.target === target)) { addError = $t('proxy.sites.already'); return; }
    custom = [...custom, { host: hostOf(target) }];
    saveCustom(custom);
    adding = '';
    if (selected.size < MAX_SELECTED) { selected.add(target); selected = selected; persistSelection(); }
  }

  function remove(t) {
    custom = custom.filter((c) => siteTarget(c.host) !== t.target);
    saveCustom(custom);
    if (selected.delete(t.target)) { selected = selected; persistSelection(); }
    delete results[t.target];
    results = results;
  }

  async function check() {
    if (!selected.size) { error = $t('proxy.sites.selectSome'); return; }
    checking = true;
    error = '';
    const targets = [...selected];
    for (const t of targets) results[t] = { pending: true };
    results = results;
    try {
      const list = (await App.CheckSites(exit, targets)) || [];
      for (const r of list) results[r.host] = r;
      results = results;
    } catch (err) {
      error = errText(err, $t('proxy.sites.failed'));
      for (const t of targets) if (results[t]?.pending) delete results[t];
      results = results;
    } finally {
      checking = false;
    }
  }

  const close = () => dispatch('close');

  function portal(node) {
    document.body.appendChild(node);
    return { destroy: () => node.remove() };
  }
</script>

<svelte:window on:keydown={(e) => e.key === 'Escape' && close()} />

<div class="layer" use:portal>
  <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
  <div class="backdrop" on:click={close}></div>
  <div class="dialog" role="dialog" aria-label={$t('proxy.checkSites')}>
    <header>
      <div class="grow">
        <h2>{$t('proxy.checkSites')}</h2>
        <p class="small muted">
          {exit === '' ? $t('proxy.sites.introDirect') : $t('proxy.sites.intro', { label: exitLabel || $t('proxy.sites.autoExit') })}
          {$t('proxy.sites.introRank')}
        </p>
      </div>
      <button class="icon" aria-label={$t('common.close')} on:click={close}><Icon name="close" size={18} /></button>
    </header>

    <div class="grid">
      {#each tiles as tile (tile.target)}
        {@const r = results[tile.target]}
        <div class="tile" class:sel={selected.has(tile.target)}>
          <button class="pick" aria-pressed={selected.has(tile.target)} title={selected.has(tile.target) ? $t('proxy.sites.selectedTitle') : $t('proxy.sites.select')} on:click={() => toggle(tile)} data-silent>
            <span class="mark" style="--c:{tile.color}">{tile.mark}</span>
            <span class="name">{tile.name}</span>
            <span class="box">{#if selected.has(tile.target)}<Icon name="check" size={12} stroke={3} />{/if}</span>
          </button>
          {#if !tile.preset}
            <button class="remove" title={$t('common.remove')} aria-label={$t('proxy.sites.removeSite', { name: tile.name })} on:click={() => remove(tile)} data-silent><Icon name="close" size={12} stroke={2.4} /></button>
          {/if}
          <div class="lines">
            <div class="line">
              <span class="faint">{$t('proxy.sites.direct')}</span>
              {#if r?.pending}<span class="spin" />{:else if r?.direct}<span class="st" class:good={r.direct.ok} class:bad={!r.direct.ok} title={r.direct.reason || ''}>{r.direct.ok ? $t('proxy.pingMs', { n: r.direct.ms }) : $t('proxy.sites.blocked')}</span>{:else}<span class="faint">—</span>{/if}
            </div>
            {#if exit !== ''}
              <div class="line">
                <span class="faint">{$t('proxy.sites.proxy')}</span>
                {#if r?.pending}<span class="spin" />{:else if r?.exit}<span class="st" class:good={r.exit.ok} class:bad={!r.exit.ok} title={r.exit.reason || ''}>{r.exit.ok ? $t('proxy.pingMs', { n: r.exit.ms }) : $t('proxy.sites.fails')}</span>{:else}<span class="faint">—</span>{/if}
              </div>
            {/if}
            {#if r && !r.pending && (!r.direct?.ok || (r.exit && !r.exit.ok))}
              <p class="why">{(!r.direct?.ok && r.direct?.reason) || ''}{r.exit && !r.exit.ok ? `${!r.direct?.ok ? '; ' : ''}${$t('proxy.sites.proxyReason', { reason: r.exit.reason })}` : ''}</p>
            {/if}
          </div>
        </div>
      {/each}
    </div>

    <form class="add row" on:submit|preventDefault={add}>
      <input class="grow" placeholder={$t('proxy.sites.addPlaceholder')} bind:value={adding} spellcheck="false" />
      <button type="submit" disabled={!adding.trim()}><Icon name="plus" size={16} /> {$t('common.add')}</button>
    </form>
    {#if addError}<p class="small error">{addError}</p>{/if}

    <footer class="row">
      <span class="small muted grow">
        {$t('proxy.sites.selected', { n: selected.size })}{#if done && !checking}{' · '}{$t('proxy.sites.directOpen', { n: openDirect })}{#if exit !== ''}, {$t('proxy.sites.proxyOpen', { n: openExit })}{/if}{/if}
      </span>
      {#if error}<span class="small error">{error}</span>{/if}
      <button class="primary" on:click={check} disabled={checking || !selected.size}>{checking ? $t('proxy.sites.checking') : $t('proxy.sites.checkNow')}</button>
    </footer>
  </div>
</div>

<style>
  .layer { position: fixed; inset: 0; z-index: 900; display: grid; place-items: center; padding: 24px; }
  .backdrop { position: absolute; inset: 0; background: var(--scrim); backdrop-filter: blur(6px); animation: fade 0.2s ease-out; }
  .dialog {
    position: relative; width: min(860px, 100%); max-height: min(86vh, 900px);
    display: flex; flex-direction: column; gap: 14px; padding: 20px;
    background: var(--panel); border: 1px solid var(--line-soft); border-radius: var(--radius-xl);
    box-shadow: var(--shadow-lg); animation: dialog-in 0.4s var(--spring);
  }
  @keyframes dialog-in { from { opacity: 0; transform: translateY(14px) scale(0.96); } to { opacity: 1; transform: none; } }
  header { display: flex; gap: 12px; align-items: flex-start; }
  header p { margin-top: 4px; max-width: 640px; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(178px, 1fr)); gap: 10px; overflow-y: auto; padding: 2px; }
  .tile {
    position: relative; display: flex; flex-direction: column; gap: 8px; padding: 10px 12px 10px;
    border-radius: 16px; background: var(--raised); border: 1.5px solid transparent;
    transition: border-color var(--dur), background var(--dur), transform 0.3s var(--spring);
  }
  .tile:hover { transform: translateY(-2px); }
  .tile.sel { border-color: var(--accent); background: var(--accent-soft); }
  .pick { display: flex; align-items: center; gap: 10px; padding: 0; background: none; border: none; text-align: left; width: 100%; }
  .pick:hover:not(:disabled) { background: none; border: none; }
  .mark {
    width: 34px; height: 34px; border-radius: 10px; flex: none; display: grid; place-items: center;
    font-weight: 750; font-size: 13px; color: #fff; letter-spacing: -0.02em;
    background: linear-gradient(135deg, var(--c), color-mix(in srgb, var(--c) 60%, #000));
    box-shadow: 0 4px 12px color-mix(in srgb, var(--c) 30%, transparent);
  }
  .name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; }
  .box {
    width: 18px; height: 18px; border-radius: 6px; flex: none; display: grid; place-items: center;
    border: 1.5px solid var(--faint); color: #fff;
  }
  .box { transition: background 0.2s, transform 0.3s var(--spring); }
  .tile.sel .box { background: linear-gradient(135deg, var(--accent), var(--accent-2)); border-color: transparent; transform: scale(1.1); }
  .remove {
    position: absolute; top: -7px; right: -7px; width: 20px; height: 20px; padding: 0; border-radius: 50%;
    display: grid; place-items: center; background: var(--elevated); border: 1px solid var(--line); color: var(--muted); opacity: 0; transition: opacity 0.15s;
  }
  .tile:hover .remove { opacity: 1; }
  .lines { display: flex; flex-direction: column; gap: 3px; font-size: 12px; }
  .line { display: flex; justify-content: space-between; align-items: center; }
  .st { font-weight: 600; padding: 1px 8px; border-radius: 999px; font-variant-numeric: tabular-nums; }
  .st.good { color: var(--ok); background: var(--ok-soft); }
  .st.bad { color: var(--danger); background: var(--danger-soft); }
  .why { color: var(--faint); font-size: 11px; line-height: 1.35; }
  .spin { width: 12px; height: 12px; border-radius: 50%; border: 2px solid var(--line); border-top-color: var(--accent); animation: spin 0.8s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
  .add button { display: inline-flex; gap: 6px; align-items: center; }
  footer { padding-top: 12px; border-top: 1px solid var(--line-soft); }
</style>
