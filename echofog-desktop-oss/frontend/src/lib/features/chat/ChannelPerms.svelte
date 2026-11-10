<script>
  import { t } from '../../i18n/index.js';
  import { PERM, hexColor } from '../../perms.js';
  import Pills from '../../components/ui/Pills.svelte';
  import Tooltip from '../../components/ui/Tooltip.svelte';

  /** What roles may do in one channel otherwise than in the group (a channel's settings): for
   *  everyone, or for one role, each of the channel's permissions forbidden here, left as the role has it
   *  in the group, or allowed here. "As the role" says what that is now. The core refuses changing a role
   *  at or above one's own rank, or granting what one may not do. */
  export let overrides = []; // [{ role, allow, deny }], role '*' for everyone
  export let roles = []; // the group's roles (GroupRoles), the owner's left out
  export let voice = false;

  const TEXT = ['send', 'media', 'links', 'threads', 'react', 'pin', 'delete'];
  const VOICE = ['voice', 'stream'];
  $: perms = voice ? VOICE : TEXT;

  let role = '*';
  $: cur = overrides.find((o) => o.role === role) || { role, allow: 0, deny: 0 };
  const stateOf = (o, p) => (o.allow & PERM[p] ? 'allow' : o.deny & PERM[p] ? 'deny' : '');
  function set(p, v) {
    const bit = PERM[p];
    const o = { role, allow: cur.allow & ~bit, deny: cur.deny & ~bit };
    if (v === 'allow') o.allow |= bit;
    if (v === 'deny') o.deny |= bit;
    overrides = [...overrides.filter((x) => x.role !== role), ...(o.allow || o.deny ? [o] : [])];
  }
  const roleName = (r) => r.title || (r.builtin ? $t('chats.role.' + r.id) : r.id);
  const changed = (id) => overrides.some((o) => o.role === id);
  // a role whose permissions here differ from the group's is marked
  $: roleItems = [{ value: '*', label: $t('chats.everyone') + (changed('*') ? ' •' : '') },
    ...roles.map((r) => ({ value: r.id, label: roleName(r) + (changed(r.id) ? ' •' : ''), color: hexColor(r.color) || 'var(--faint)' }))];

  // what "as the role" means: the group's permissions of the chosen role (for everyone: of a plain member)
  $: base = role === '*' ? roles.find((r) => r.id === 'member') : roles.find((r) => r.id === role);
  const inherited = (b, p) => (b && typeof b.perms === 'number' ? (b.perms & PERM[p] ? 'allow' : 'deny') : '');
  $: baseName = role === '*' ? $t('chats.role.member') : base ? roleName(base) : '';
</script>

<div class="cperms">
  <p class="small muted">{$t('cset.permsIntro')}</p>
  <Pills items={roleItems} bind:value={role} label={$t('chats.channelPerms')} />
  <div class="rows">
    {#each perms as p (p)}
      {@const st = stateOf(cur, p)}
      {@const inh = inherited(base, p)}
      <div class="cp-row">
        <div class="grow">
          <span class="nm">{$t('chats.perm.' + p)}</span>
          {#if st === ''}<span class="now small" class:ok={inh === 'allow'} class:no={inh === 'deny'}>{inh ? $t('cset.now.' + inh) : $t('cset.nowUnknown')}</span>{/if}
        </div>
        <div class="tri" role="radiogroup" aria-label={$t('chats.perm.' + p)}>
          <Tooltip text={$t('cset.denyTip')}><button type="button" role="radio" aria-checked={st === 'deny'} class="deny" class:on={st === 'deny'} on:click={() => set(p, 'deny')}>{$t('cset.deny')}</button></Tooltip>
          <Tooltip text={$t('cset.inheritTip', { role: baseName || $t('chats.everyone') })}><button type="button" role="radio" aria-checked={st === ''} class:on={st === ''} on:click={() => set(p, '')}>{$t('cset.inherit')}</button></Tooltip>
          <Tooltip text={$t('cset.allowTip')}><button type="button" role="radio" aria-checked={st === 'allow'} class="allow" class:on={st === 'allow'} on:click={() => set(p, 'allow')}>{$t('cset.allow')}</button></Tooltip>
        </div>
      </div>
    {/each}
  </div>
  <p class="small faint">{$t('chats.channelPermsHint')}</p>
</div>

<style>
  .cperms { display: flex; flex-direction: column; gap: var(--sp-3); }
  .rows { display: flex; flex-direction: column; border: 1px solid var(--border); border-radius: var(--r-lg); overflow: hidden; }
  .cp-row { display: flex; align-items: center; gap: var(--sp-3); min-height: 48px; padding: 6px var(--sp-3); }
  .cp-row + .cp-row { border-top: 1px solid var(--border); }
  .nm { display: block; font-size: var(--fs-md); }
  .now { color: var(--faint); }
  .now.ok { color: var(--ok-fg); }
  .now.no { color: var(--danger-fg); }
  .tri { display: inline-flex; padding: 2px; gap: 2px; border-radius: var(--r-lg); background: var(--surface-2); flex: none; }
  .tri button { height: 28px; padding: 0 var(--sp-3); border: 0; border-radius: var(--r-md); background: none; color: var(--muted); font-size: var(--fs-sm); font-weight: var(--fw-medium); white-space: nowrap; }
  .tri button:hover:not(.on):not(:disabled) { color: var(--text); background: var(--hover); border: 0; }
  .tri button.on { background: var(--surface-3); color: var(--text); box-shadow: var(--shadow-sm); }
  .tri button.deny.on { background: var(--danger-soft); color: var(--danger-fg); }
  .tri button.allow.on { background: var(--ok-soft); color: var(--ok-fg); }
</style>
