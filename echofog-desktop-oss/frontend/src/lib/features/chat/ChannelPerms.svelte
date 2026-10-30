<script>
  import { t } from '../../i18n/index.js';
  import { PERM, hexColor } from '../../perms.js';
  import Pills from '../../components/ui/Pills.svelte';

  /** What roles may do in one channel otherwise than in the group (a channel's settings): for
   *  everyone, or for one role, each of the channel's permissions taken, as in the group, or
   *  granted. The core refuses changing a role at or above one's own rank, or granting what one
   *  may not do. */
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
</script>

<div class="cperms">
  <span class="small faint">{$t('chats.channelPerms')}</span>
  <Pills items={roleItems} bind:value={role} label={$t('chats.channelPerms')} />
  {#each perms as p (p)}
    <div class="cp-row">
      <span class="grow small">{$t('chats.perm.' + p)}</span>
      <div class="tri" role="radiogroup" aria-label={$t('chats.perm.' + p)}>
        <button type="button" class="deny" class:on={stateOf(cur, p) === 'deny'} title={$t('chats.permDeny')} aria-label={$t('chats.permDeny')} on:click={() => set(p, 'deny')}>✕</button>
        <button type="button" class:on={stateOf(cur, p) === ''} title={$t('chats.permInherit')} aria-label={$t('chats.permInherit')} on:click={() => set(p, '')}>/</button>
        <button type="button" class="allow" class:on={stateOf(cur, p) === 'allow'} title={$t('chats.permAllow')} aria-label={$t('chats.permAllow')} on:click={() => set(p, 'allow')}>✓</button>
      </div>
    </div>
  {/each}
  <p class="small faint">{$t('chats.channelPermsHint')}</p>
</div>

<style>
  .cperms { display: flex; flex-direction: column; gap: 4px; margin-top: 8px; padding-top: 8px; border-top: 1px solid var(--border, rgba(127, 127, 127, 0.2)); }
  .cp-row { display: flex; align-items: center; gap: 8px; min-height: 26px; }
  .tri { display: inline-flex; border-radius: 7px; overflow: hidden; border: 1px solid var(--border, rgba(127, 127, 127, 0.3)); flex: none; }
  .tri button { width: 26px; height: 22px; padding: 0; border: 0; border-radius: 0; background: none; color: var(--faint); font-size: 12px; line-height: 22px; cursor: pointer; }
  .tri button + button { border-left: 1px solid var(--border, rgba(127, 127, 127, 0.3)); }
  .tri button.on { background: var(--accent-soft); color: var(--text); }
  .tri button.deny.on { background: rgba(229, 72, 77, 0.22); color: #e5484d; }
  .tri button.allow.on { background: rgba(70, 167, 88, 0.22); color: #46a758; }
</style>
