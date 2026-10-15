<script>
  /**
   * Shown on the relay card when this computer is not reachable from the Internet. It explains why
   * (from st.code) and gives step-by-step ways to fix it, plus the reassurance that chatting,
   * calls and using the VPN all work anyway — only being a relay/exit for others needs reachability.
   */
  import { t } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  export let code = ''; // "connecting" | "nomap" | "cgnat" | "symmetric" | ""
  export let reason = '';
  export let port = 0; // the fixed listen port, 0 = random

  let open = false;

  // which remedies apply, in order, for each reason
  const PLANS = {
    connecting: [],
    nomap: ['upnp', 'forward', 'white', 'ipv6', 'vps'],
    cgnat: ['white', 'vps', 'ipv6'],
    symmetric: ['white', 'vps', 'forward'],
  };
  $: steps = PLANS[code] || ['upnp', 'forward', 'white', 'vps'];
  $: title = code === 'connecting' ? $t('nat.connecting') : $t('nat.title');
</script>

<div class="nat" class:plain={code === 'connecting'}>
  <div class="head">
    <span class="ic" class:spin={code === 'connecting'}><Icon name={code === 'connecting' ? 'refresh' : 'globe'} size={17} /></span>
    <div class="grow">
      <b>{title}</b>
      {#if !code}<p class="small muted">{reason || $t('relay.unreachable')}</p>{/if}
    </div>
  </div>

  {#if steps.length}
    <p class="small ok works"><Icon name="check" size={14} /> {$t('nat.worksAnyway')}</p>
    <button class="toggle small" on:click={() => (open = !open)}>
      <Icon name={open ? 'chevronDown' : 'chevronRight'} size={14} /> {open ? $t('nat.hide') : $t('nat.howTo')}
    </button>
    {#if open}
      <ol class="steps">
        {#each steps as s}
          <li>
            <b>{$t('nat.' + s + '.t')}</b>
            <span class="small muted">{s === 'forward' ? $t('nat.forward.d', { port: port || $t('nat.forward.any') }) : $t('nat.' + s + '.d')}</span>
          </li>
        {/each}
      </ol>
      <p class="small faint">{$t('nat.footer')}</p>
    {/if}
  {/if}
</div>

<style>
  .nat { border-radius: 14px; background: var(--warn-soft); border-left: 3px solid var(--warn); padding: 12px 14px; display: flex; flex-direction: column; gap: 10px; }
  .nat.plain { background: var(--raised); border-left-color: var(--line); }
  .head { display: flex; gap: 11px; align-items: flex-start; }
  .ic { width: 30px; height: 30px; border-radius: 10px; flex: none; display: grid; place-items: center; background: var(--warn-soft); color: var(--warn); }
  .nat.plain .ic { background: var(--accent-soft); color: var(--accent); }
  .ic.spin :global(svg) { animation: spin 1.2s linear infinite; }
  .head p { margin-top: 2px; }
  .works { display: inline-flex; align-items: center; gap: 6px; }
  .toggle { align-self: flex-start; display: inline-flex; align-items: center; gap: 6px; background: transparent; border-color: transparent; color: var(--accent); padding: 2px 4px; }
  .toggle:hover:not(:disabled) { background: var(--hover); border-color: transparent; }
  .steps { margin: 0; padding-left: 20px; display: flex; flex-direction: column; gap: 9px; }
  .steps li { line-height: 1.4; }
  .steps li::marker { color: var(--accent); font-weight: 700; }
  .steps b { display: block; font-size: 13px; }
  .steps span { display: block; }
</style>
