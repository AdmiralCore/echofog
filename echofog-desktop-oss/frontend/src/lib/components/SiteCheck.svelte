<script>
  /**
   * Which sites open directly and which through the proxy. Selected tiles are checked, and they are
   * also the sites the app uses to rank exits for the automatic choice. Both lists — the selected
   * sites and the ones the user added — are kept by the node, so they survive restarts.
   */
  import { onMount, onDestroy, tick, createEventDispatcher } from 'svelte';
  import { App, errText } from '../api.js';
  import Icon from './ui/Icon.svelte';
  import Button from './ui/Button.svelte';
  import IconButton from './ui/IconButton.svelte';
  import Input from './ui/Input.svelte';
  import Sheet from './ui/Sheet.svelte';
  import { PRESET_SITES, MAX_SELECTED, siteTarget, hostOf, loadCustom, saveCustom, splitSites } from '../sites.js';
  import { t } from '../i18n/index.js';

  export let exit = 'auto'; // peer id, 'auto', or '' for the direct check only
  export let exitLabel = ''; // defaults to "the automatic exit"

  const dispatch = createEventDispatcher();
  const presetTargets = new Set(PRESET_SITES.map((p) => siteTarget(p.host)));

  let custom = loadCustom().map((c) => siteTarget(c.host)).filter(Boolean); // added sites, host:port
  let selected = new Set();
  let results = {}; // target -> { direct, exit }
  let checking = false;
  let error = '';
  let adding = '';
  let addError = '';
  let fresh = new Set(); // tiles just added, highlighted for a moment
  let saveTimer;
  let gridEl;

  $: tiles = [
    ...PRESET_SITES.map((p) => ({ ...p, target: siteTarget(p.host), preset: true })),
    ...custom.map((target) => {
      const name = hostOf(target).replace(/^www\./, '');
      return { name, host: hostOf(target), target, mark: name.slice(0, 2), color: colorFor(name), preset: false };
    }),
  ].filter((t) => t.target);
  $: done = Object.keys(results).length;
  $: openDirect = tiles.filter((t) => results[t.target]?.direct?.ok).length;
  $: openExit = tiles.filter((t) => results[t.target]?.exit?.ok).length;

  function colorFor(s) {
    let h = 0;
    for (const ch of s) h = (h * 31 + ch.charCodeAt(0)) % 360;
    return `hsl(${h} 55% 55%)`;
  }

  // what a new user checks first: popular services
  const starter = () => new Set(PRESET_SITES.filter((p) => ['YouTube', 'Discord', 'Instagram', 'X', 'WhatsApp', 'Telegram'].includes(p.name)).map((p) => siteTarget(p.host)));

  onMount(async () => {
    let lists = null;
    try { lists = await App.GetSiteLists(); } catch (_) { /* an older core, or the preview */ }
    if (!lists) return legacyLoad();
    // sites added by older versions lived only in this window's storage: move them to the node
    const merged = [...(lists.custom || []), ...custom].filter((d) => d && !presetTargets.has(d));
    // a selected site that is not a tile yet becomes an added one
    for (const d of lists.selected || []) if (!presetTargets.has(d)) merged.push(d);
    const unique = [...new Set(merged)];
    custom = unique;
    if (unique.length !== (lists.custom || []).length) saveCustomSites();
    if (lists.chosen) {
      selected = new Set(lists.selected || []);
    } else {
      selected = starter();
      persistSelection(true);
    }
  });

  async function legacyLoad() {
    try {
      const domains = (await App.GetProbeDomains()) || [];
      for (const d of domains) if (!presetTargets.has(d) && !custom.includes(d)) custom = [...custom, d];
      selected = domains.length ? new Set(domains) : starter();
    } catch (_) {
      selected = starter();
    }
  }

  // a selection change is saved shortly after the last click, and at once when the dialog closes
  function persistSelection(now = false) {
    clearTimeout(saveTimer);
    const save = async () => {
      saveTimer = null;
      try { await App.SetProbeDomains([...selected]); } catch (err) { error = errText(err); }
    };
    if (now) save();
    else saveTimer = setTimeout(save, 400);
  }

  onDestroy(() => { if (saveTimer) persistSelection(true); });

  async function saveCustomSites() {
    saveCustom(custom.map((host) => ({ host }))); // a copy in this window, for an older core
    try { await App.SetCustomSites(custom); } catch (_) { /* an older core keeps them in this window only */ }
  }

  function toggle(tile) {
    error = '';
    if (selected.has(tile.target)) selected.delete(tile.target);
    else if (selected.size >= MAX_SELECTED) { error = $t('proxy.sites.tooMany', { n: MAX_SELECTED }); return; }
    else selected.add(tile.target);
    selected = selected;
    persistSelection();
  }

  // Add one site or several at once ("vk.com ya.ru, gosuslugi.ru" or a pasted list): each is
  // added, selected while there is room, and checked right away.
  async function add() {
    addError = '';
    const bad = [], added = [];
    for (const s of splitSites(adding)) {
      const target = siteTarget(s);
      if (!target) bad.push(s);
      else if (!tiles.some((t) => t.target === target) && !added.includes(target)) added.push(target);
    }
    if (!added.length) {
      addError = bad.length ? $t('proxy.sites.typeSite') : $t('proxy.sites.already');
      return;
    }
    custom = [...custom, ...added];
    saveCustomSites();
    let full = false;
    for (const target of added) {
      if (selected.size < MAX_SELECTED) selected.add(target);
      else full = true;
    }
    selected = selected;
    persistSelection();
    adding = '';
    if (bad.length) addError = $t('proxy.sites.notSites', { list: bad.join(', ') });
    else if (full) addError = $t('proxy.sites.addedNotSelected', { n: MAX_SELECTED });
    fresh = new Set(added);
    setTimeout(() => { fresh = new Set(); }, 1600);
    await tick();
    gridEl?.querySelector(`[data-target="${CSS.escape(added[added.length - 1])}"]`)?.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    const toCheck = added.filter((d) => selected.has(d));
    if (toCheck.length && !checking) runCheck(toCheck);
  }

  function remove(t) {
    custom = custom.filter((d) => d !== t.target);
    saveCustomSites();
    if (selected.delete(t.target)) { selected = selected; persistSelection(); }
    delete results[t.target];
    results = results;
  }

  function check() {
    if (!selected.size) { error = $t('proxy.sites.selectSome'); return; }
    runCheck([...selected]);
  }

  async function runCheck(targets) {
    checking = true;
    error = '';
    for (const t of targets) results[t] = { pending: true };
    results = results;
    try {
      const list = (await App.CheckSites(exit, targets)) || [];
      for (const r of list) results[r.host] = r;
    } catch (err) {
      error = errText(err, $t('proxy.sites.failed'));
    } finally {
      for (const t of targets) if (results[t]?.pending) delete results[t];
      results = results;
      checking = false;
    }
  }

  // Why a site did not open, in the user's language; the raw reason stays in the tooltip.
  const WHY = ['payment', 'cert', 'timeout', 'reset', 'refused', 'exit'];
  function whyText(reach, path) {
    if (!reach || reach.ok) return '';
    const code = reach.code || '';
    if (code === 'dns') return $t(path === 'direct' ? 'proxy.sites.why.dnsDirect' : 'proxy.sites.why.dnsExit');
    if (WHY.includes(code)) return $t('proxy.sites.why.' + code);
    if (code === 'other') return $t('proxy.sites.why.other');
    return reach.reason || ''; // an older core sends no code
  }
  // the short badge: what blocking looks like, or just "did not open"
  const looksBlocked = (reach) => ['dns', 'cert', 'timeout', 'reset'].includes(reach?.code || 'reset');

  const close = () => dispatch('close');
</script>

<Sheet title={$t('proxy.checkSites')} width={860} on:close={close}
  hint={(exit === '' ? $t('proxy.sites.introDirect') : $t('proxy.sites.intro', { label: exitLabel || $t('proxy.sites.autoExit') })) + ' ' + $t('proxy.sites.introRank')}>
  <div class="grid" bind:this={gridEl}>
    {#each tiles as tile (tile.target)}
      {@const r = results[tile.target]}
      <div class="tile" class:sel={selected.has(tile.target)} class:fresh={fresh.has(tile.target)} data-target={tile.target}>
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
            {#if r?.pending}<span class="spin"></span>{:else if r?.direct}<span class="st" class:good={r.direct.ok} class:bad={!r.direct.ok} title={r.direct.reason || ''}>{r.direct.ok ? $t('proxy.pingMs', { n: r.direct.ms }) : looksBlocked(r.direct) ? $t('proxy.sites.blocked') : $t('proxy.sites.fails')}</span>{:else}<span class="faint">—</span>{/if}
          </div>
          {#if exit !== ''}
            <div class="line">
              <span class="faint">{$t('proxy.sites.proxy')}</span>
              {#if r?.pending}<span class="spin"></span>{:else if r?.exit}<span class="st" class:good={r.exit.ok} class:bad={!r.exit.ok} title={r.exit.reason || ''}>{r.exit.ok ? $t('proxy.pingMs', { n: r.exit.ms }) : $t('proxy.sites.fails')}</span>{:else}<span class="faint">—</span>{/if}
            </div>
          {/if}
          {#if r && !r.pending && (!r.direct?.ok || (r.exit && !r.exit.ok))}
            {@const dw = whyText(r.direct, 'direct')}
            {@const ew = r.exit ? whyText(r.exit, 'exit') : ''}
            <p class="why" title={[r.direct?.reason, r.exit?.reason].filter(Boolean).join(' · ')}>{dw}{dw && ew ? '; ' : ''}{ew ? $t('proxy.sites.proxyReason', { reason: ew }) : ''}</p>
          {/if}
        </div>
      </div>
    {/each}
  </div>

  <form class="add" on:submit|preventDefault={add}>
    <div class="grow"><Input aria-label={$t('proxy.sites.addPlaceholder')} placeholder={$t('proxy.sites.addPlaceholder')} bind:value={adding} on:input={() => (addError = '')} spellcheck="false" autocomplete="off" /></div>
    <Button type="submit" icon="plus" disabled={!adding.trim()}>{$t('common.add')}</Button>
  </form>
  {#if addError}<p class="small error" role="alert">{addError}</p>{/if}

  <svelte:fragment slot="footer">
    <span class="small muted grow">
      {$t('proxy.sites.selected', { n: selected.size })}{#if done && !checking}{' · '}{$t('proxy.sites.directOpen', { n: openDirect })}{#if exit !== ''}, {$t('proxy.sites.proxyOpen', { n: openExit })}{/if}{/if}
    </span>
    {#if error}<span class="small error" role="alert">{error}</span>{/if}
    <Button variant="primary" on:click={check} loading={checking} disabled={!selected.size}>{checking ? $t('proxy.sites.checking') : $t('proxy.sites.checkNow')}</Button>
  </svelte:fragment>
</Sheet>

<style>
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(178px, 1fr)); gap: var(--sp-2); padding: 2px; }
  .tile {
    position: relative; display: flex; flex-direction: column; gap: var(--sp-2); padding: var(--sp-2) var(--sp-3);
    border-radius: var(--r-lg); background: var(--surface-2); border: 1px solid var(--border);
    transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease);
  }
  .tile:hover { border-color: var(--border-strong); }
  .tile.sel { border-color: var(--accent); background: var(--accent-soft); }
  .tile.fresh { animation: fresh 1.6s ease-out; }
  @keyframes fresh { 0%, 30% { box-shadow: 0 0 0 3px var(--accent); } 100% { box-shadow: 0 0 0 0 transparent; } }
  .pick { display: flex; align-items: center; gap: var(--sp-2); padding: 0; height: auto; background: none; border: none; text-align: left; width: 100%; }
  .pick:hover:not(:disabled) { background: none; border: none; }
  .mark {
    width: 30px; height: 30px; border-radius: var(--r-md); flex: none; display: grid; place-items: center;
    font-weight: var(--fw-semibold); font-size: var(--fs-xs); color: #fff; letter-spacing: -0.02em; background: var(--c);
  }
  .name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: var(--fw-medium); }
  .box { width: 18px; height: 18px; border-radius: var(--r-xs); flex: none; display: grid; place-items: center; border: 1.5px solid var(--faint); color: var(--accent-ink); transition: background var(--dur) var(--ease); }
  .tile.sel .box { background: var(--accent); border-color: var(--accent); }
  .remove {
    position: absolute; top: -7px; right: -7px; width: 20px; height: 20px; padding: 0; border-radius: 50%;
    display: grid; place-items: center; background: var(--surface-3); border: 1px solid var(--border-strong); color: var(--muted); opacity: 0; transition: opacity 0.15s;
  }
  .tile:hover .remove, .remove:focus-visible { opacity: 1; }
  @media (hover: none) { .remove { opacity: 1; } }
  .lines { display: flex; flex-direction: column; gap: 3px; font-size: var(--fs-xs); }
  .line { display: flex; justify-content: space-between; align-items: center; }
  .st { font-weight: var(--fw-semibold); padding: 1px 8px; border-radius: var(--r-pill); font-variant-numeric: tabular-nums; }
  .st.good { color: var(--ok-fg); background: var(--ok-soft); }
  .st.bad { color: var(--danger-fg); background: var(--danger-soft); }
  .why { color: var(--faint); font-size: var(--fs-2xs); line-height: 1.35; }
  .spin { width: 12px; height: 12px; border-radius: 50%; border: 2px solid var(--border-strong); border-top-color: var(--accent); animation: spin 0.8s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
  .add { display: flex; gap: var(--sp-2); align-items: flex-start; }
</style>
