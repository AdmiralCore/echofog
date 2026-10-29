<script>
  import { createEventDispatcher } from 'svelte';
  import { App, errText } from '../../api.js';
  import { membersVersion } from '../../stores/chats.js';
  import { t } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import Icon from '../../components/ui/Icon.svelte';
  import { PERM, PERM_GROUPS, ROLE_COLORS, hexColor } from '../../perms.js';

  /** A group's roles in its side panel (ChatInfo): the built-in ones and its own, with how many hold
   *  each. Who may manage roles (canEdit per role, from the core) changes those below their rank and
   *  defines new ones; the core refuses granting what they may not do themselves. */
  export let conv;
  /** The roles as loaded, for the member list's names and colours. */
  export let roles = [];

  const dispatch = createEventDispatcher();
  let error = '';
  let editing = null; // { id, title, color, rank, perms, builtin, canRemove }
  let busy = false;

  $: void $membersVersion, load(conv.groupId);
  async function load(id) {
    try {
      roles = (await App.GroupRoles(id)) || [];
      dispatch('roles', roles);
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }

  const roleName = (r) => (r.title || (r.builtin ? $t('chats.role.' + r.id) : r.id));

  // a new role starts just above the highest role the editor may change (the core keeps it below theirs)
  function startNew() {
    const mine = roles.filter((r) => r.canEdit).reduce((m, r) => Math.max(m, r.rank), 0);
    editing = { id: '', title: '', color: ROLE_COLORS[6], rank: Math.max(2, Math.min(mine + 1, 40)), perms: PERM.send | PERM.media | PERM.voice | PERM.links | PERM.threads | PERM.react | PERM.invite, builtin: false, canRemove: false };
  }
  const startEdit = (r) => (editing = { ...r });
  const toggle = (p) => (editing.perms ^= PERM[p]);

  async function save() {
    if (!editing.builtin && !editing.title.trim()) return;
    busy = true;
    try {
      await App.GroupRoleSave(conv.groupId, { ...editing, title: editing.title.trim(), rank: +editing.rank || 0, members: 0, canEdit: false, canRemove: false });
      editing = null;
      membersVersion.update((n) => n + 1);
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }
  async function remove() {
    const r = editing;
    if (!(await ask({ message: $t('chats.confirm.removeRole', { name: roleName(r) }), confirm: $t('common.remove'), danger: true }))) return;
    busy = true;
    try {
      await App.GroupRoleRemove(conv.groupId, r.id);
      editing = null;
      membersVersion.update((n) => n + 1);
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="block">
  <div class="row roles-head">
    <span class="eyebrow grow">{$t('chats.roles')}</span>
    {#if conv.can.roles && !editing}<button class="ghost small with-icon" on:click={startNew}><Icon name="plus" size={14} /> {$t('chats.newRole')}</button>{/if}
  </div>
  {#if editing}
    <form class="role-edit" on:submit|preventDefault={save}>
      <label class="small faint" for="role-name">{$t('chats.roleName')}</label>
      <input id="role-name" bind:value={editing.title} maxlength="32" placeholder={editing.builtin ? $t('chats.role.' + editing.id) : ''} />
      <span class="small faint">{$t('chats.roleColor')}</span>
      <div class="swatches">
        {#each ROLE_COLORS as c}
          <button type="button" class="swatch" class:on={editing.color === c} class:none={!c} style:background={hexColor(c) || null}
            title={c ? hexColor(c) : $t('chats.roleNoColor')} aria-label={c ? hexColor(c) : $t('chats.roleNoColor')} on:click={() => (editing.color = c)}></button>
        {/each}
      </div>
      {#if !editing.builtin}
        <label class="small faint" for="role-rank">{$t('chats.roleRank')}</label>
        <input id="role-rank" type="number" min="1" max="99" bind:value={editing.rank} />
        <p class="small faint">{$t('chats.roleRankHint')}</p>
      {/if}
      {#each PERM_GROUPS as g}
        <div class="perm-group">
          <span class="small faint">{$t('chats.permGroup.' + g.key)}</span>
          {#each g.perms as p}
            <label class="check"><input type="checkbox" checked={(editing.perms & PERM[p]) !== 0} on:change={() => toggle(p)} /> <span>{$t('chats.perm.' + p)}</span></label>
          {/each}
        </div>
      {/each}
      <div class="row role-actions">
        {#if editing.canRemove}<button type="button" class="danger small" disabled={busy} on:click={remove}>{$t('common.remove')}</button>{/if}
        <span class="grow"></span>
        <button type="button" class="ghost small" on:click={() => (editing = null)}>{$t('common.cancel')}</button>
        <button type="submit" class="primary small" disabled={busy || (!editing.builtin && !editing.title.trim())}>{$t('common.save')}</button>
      </div>
    </form>
  {:else}
    <div class="role-list">
      {#each roles as r (r.id)}
        <div class="role-row">
          <span class="role-dot" style:background={hexColor(r.color) || null}></span>
          <span class="grow nm">{roleName(r)}</span>
          <span class="small faint">{r.members}</span>
          {#if r.canEdit}<button class="icon tiny" title={$t('chats.editRole')} aria-label={$t('chats.editRole')} on:click={() => startEdit(r)}><Icon name="edit" size={13} /></button>{/if}
        </div>
      {/each}
    </div>
  {/if}
  {#if error}<p class="error small">{error}</p>{/if}
</div>

<style>
  .roles-head { align-items: center; gap: 6px; }
  .role-list { display: flex; flex-direction: column; gap: 2px; margin-top: 6px; }
  .role-row { display: flex; align-items: center; gap: 8px; min-height: 30px; padding: 0 4px 0 6px; border-radius: 8px; }
  .role-row:hover { background: var(--hover, rgba(127, 127, 127, 0.08)); }
  .role-row .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }
  .role-dot { width: 10px; height: 10px; border-radius: 50%; flex: none; background: var(--faint); opacity: 0.9; }
  .role-edit { display: flex; flex-direction: column; gap: 6px; margin-top: 8px; }
  .swatches { display: flex; flex-wrap: wrap; gap: 6px; }
  .swatch { width: 22px; height: 22px; padding: 0; border-radius: 50%; border: 2px solid transparent; cursor: pointer; }
  .swatch.none { background: transparent; border-color: var(--border, rgba(127, 127, 127, 0.4)); border-style: dashed; }
  .swatch.on { outline: 2px solid var(--accent); outline-offset: 2px; }
  .perm-group { display: flex; flex-direction: column; gap: 2px; margin-top: 6px; }
  .check { display: flex; align-items: center; gap: 8px; font-size: 13px; }
  .check input { width: auto; }
  .role-actions { margin-top: 8px; gap: 6px; align-items: center; }
</style>
