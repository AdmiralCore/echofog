<script>
  import { App, errText, copy } from '../../api.js';
  import { webLinkOf } from '../../links.js';
  import { t, intlLocale } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import { membersVersion } from '../../stores/chats.js';
  import Icon from '../../components/ui/Icon.svelte';

  /** A group's invitations in its side panel (ChatInfo). The quick one (GetGroupInvite, a week)
   *  in its https form, which messengers make tappable, and, in a private group, the invitations
   *  made on its log: how long each lets people in, how many, with which role; its maker sees its
   *  link again and anyone may end their own (who manages roles, anybody's). */
  export let conv;
  export let roles = []; // the group's roles (GroupRoles)

  const TERMS = [1, 24, 24 * 7, 24 * 30]; // hours
  const USES = [0, 1, 5, 10, 25, 100];
  let quick = '';
  let list = null; // null: the group takes no managed invitations (an open one, an older core)
  let error = '';
  let copied = '';
  let making = null; // { label, hours, uses, role }
  let busy = false;

  $: void $membersVersion, load(conv.groupId);
  async function load(id) {
    error = '';
    try { quick = conv.can.invite ? await App.GetGroupInvite(id) : ''; } catch (err) { quick = ''; }
    try { list = (await App.GroupInvites(id)) || []; } catch (_) { list = null; }
  }
  $: quickWeb = webLinkOf(quick);
  $: givable = roles.filter((r) => r.canEdit && r.rank < 100);

  async function doCopy(what, value) {
    if (await copy(value)) {
      copied = what;
      setTimeout(() => (copied = ''), 1500);
    }
  }
  async function create() {
    busy = true;
    try {
      const inv = await App.GroupInviteCreate(conv.groupId, { label: making.label.trim(), hours: +making.hours, uses: +making.uses, role: making.role, approval: !!making.approval });
      making = null;
      await load(conv.groupId);
      doCopy(inv.n, inv.webLink || inv.link);
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }
  // the group in a file (.fogzip): its invitation and history, for somebody no member can reach now
  let saving = null; // { pass, again }
  let savedTo = '';
  async function saveFile() {
    busy = true;
    error = '';
    try {
      const path = await App.GroupFileSave(conv.groupId, conv.name, saving.pass);
      if (path) { savedTo = path; saving = null; }
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  async function revoke(inv) {
    if (!(await ask({ message: $t('chats.confirm.revokeInvite'), confirm: $t('chats.revoke'), danger: true }))) return;
    try {
      await App.GroupInviteRevoke(conv.groupId, inv.n);
      await load(conv.groupId);
    } catch (err) {
      error = errText(err);
    }
  }
  const termLabel = (h) => (h < 24 ? $t('chats.inv.hours', { n: h }) : $t('chats.inv.days', { n: h / 24 }));
  const when = (s) => new Date(s * 1000).toLocaleString(intlLocale(), { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
  const roleName = (id) => {
    const r = roles.find((x) => x.id === id);
    return r?.title || (r && !r.builtin ? id : $t('chats.role.' + id));
  };
</script>

{#if quick || list}
  <div class="block">
    <div class="row inv-head">
      <span class="eyebrow grow">{$t('chats.invite')}</span>
      {#if list && conv.can.invite && !making}
        <button class="ghost small with-icon" on:click={() => (making = { label: '', hours: 24 * 7, uses: 0, role: '', approval: false })}><Icon name="plus" size={14} /> {$t('chats.inv.new')}</button>
      {/if}
    </div>
    {#if quick && !making}
      <div class="copybox">
        <span class="mono grow clip">{quickWeb || quick}</span>
        <button class="icon" title={$t('links.copyWeb')} on:click={() => doCopy('quick', quickWeb || quick)}><Icon name={copied === 'quick' ? 'check' : 'copy'} size={16} /></button>
      </div>
      <p class="small faint">{$t(list ? 'chats.inv.quickHint' : 'chats.inviteHint')}</p>
      {#if quickWeb}<button class="link-btn small" on:click={() => doCopy('quickApp', quick)}>{copied === 'quickApp' ? $t('chats.inv.copied') : $t('chats.inv.copyApp')}</button>{/if}
    {/if}

    {#if making}
      <form class="inv-make" on:submit|preventDefault={create}>
        <label class="small faint" for="inv-label">{$t('chats.inv.label')}</label>
        <input id="inv-label" bind:value={making.label} maxlength="64" placeholder={$t('chats.inv.labelHint')} />
        <div class="row inv-opts">
          <label class="small faint grow">{$t('chats.inv.term')}
            <select bind:value={making.hours}>{#each TERMS as h}<option value={h}>{termLabel(h)}</option>{/each}</select>
          </label>
          <label class="small faint grow">{$t('chats.inv.uses')}
            <select bind:value={making.uses}>{#each USES as u}<option value={u}>{u ? u : $t('chats.inv.unlimited')}</option>{/each}</select>
          </label>
        </div>
        <label class="check"><input type="checkbox" bind:checked={making.approval} /> <span>{$t('chats.inv.approval')}</span></label>
        {#if givable.length}
          <label class="small faint" for="inv-role">{$t('chats.inv.role')}</label>
          <select id="inv-role" bind:value={making.role}>
            <option value="">{$t('chats.inv.defaultRole')}</option>
            {#each givable as r (r.id)}<option value={r.id}>{roleName(r.id)}</option>{/each}
          </select>
        {/if}
        <div class="row inv-actions">
          <span class="grow"></span>
          <button type="button" class="ghost small" on:click={() => (making = null)}>{$t('common.cancel')}</button>
          <button type="submit" class="primary small" disabled={busy}>{$t('chats.inv.create')}</button>
        </div>
      </form>
    {/if}

    {#if list && !making}
      {#if saving}
        <form class="inv-make" on:submit|preventDefault={saveFile}>
          <span class="small faint">{$t('chats.file.hint')}</span>
          <input type="password" bind:value={saving.pass} placeholder={$t('chats.file.pass')} autocomplete="new-password" />
          <input type="password" bind:value={saving.again} placeholder={$t('chats.file.again')} autocomplete="new-password" />
          <div class="row inv-actions">
            <span class="grow"></span>
            <button type="button" class="ghost small" on:click={() => (saving = null)}>{$t('common.cancel')}</button>
            <button type="submit" class="primary small" disabled={busy || (saving.pass || '').length < 8 || saving.pass !== saving.again}>{$t('chats.file.save')}</button>
          </div>
        </form>
      {:else}
        <button class="link-btn small" on:click={() => { saving = { pass: '', again: '' }; savedTo = ''; }}>{$t('chats.file.start')}</button>
        {#if savedTo}<p class="small faint">{$t('chats.file.saved', { path: savedTo })}</p>{/if}
      {/if}
    {/if}

    {#if list && list.length}
      <div class="inv-list">
        {#each list as inv (inv.n)}
          <div class="inv-row" class:off={inv.state !== 'active'}>
            <div class="grow inv-meta">
              <span class="nm">{inv.label || $t('chats.inv.noLabel')}</span>
              <span class="small faint">
                {$t('chats.inv.st.' + inv.state)} · {inv.uses ? $t('chats.inv.usedOf', { n: inv.used, of: inv.uses }) : $t('chats.inv.used', { n: inv.used })}
                {#if inv.state === 'active'}{' · '}{$t('chats.inv.until', { when: when(inv.exp) })}{/if}
                {#if inv.role}{' · '}{roleName(inv.role)}{/if}
                {#if inv.approval}{' · '}{$t('chats.inv.withApproval')}{/if}
              </span>
            </div>
            {#if inv.state === 'active' && (inv.webLink || inv.link)}
              <button class="icon tiny" title={$t('links.copyWeb')} aria-label={$t('links.copyWeb')} on:click={() => doCopy(inv.n, inv.webLink || inv.link)}><Icon name={copied === inv.n ? 'check' : 'copy'} size={14} /></button>
            {/if}
            {#if inv.canRevoke && inv.state === 'active'}
              <button class="icon tiny" title={$t('chats.revoke')} aria-label={$t('chats.revoke')} on:click={() => revoke(inv)}><Icon name="close" size={14} /></button>
            {/if}
          </div>
        {/each}
      </div>
    {/if}
    {#if error}<p class="error small">{error}</p>{/if}
  </div>
{/if}

<style>
  .inv-head { align-items: center; gap: 6px; }
  .link-btn { background: none; border: 0; padding: 2px 0; color: var(--accent); cursor: pointer; text-align: left; }
  .inv-make { display: flex; flex-direction: column; gap: 6px; margin-top: 8px; }
  .inv-opts { gap: 8px; }
  .check { display: flex; align-items: center; gap: 8px; font-size: 13px; margin-top: 4px; }
  .check input { width: auto; }
  .inv-opts label { display: flex; flex-direction: column; gap: 4px; }
  .inv-actions { margin-top: 6px; gap: 6px; align-items: center; }
  .inv-list { display: flex; flex-direction: column; gap: 2px; margin-top: 10px; }
  .inv-row { display: flex; align-items: center; gap: 6px; min-height: 36px; padding: 2px 4px; border-radius: 8px; }
  .inv-row.off { opacity: 0.55; }
  .inv-meta { display: flex; flex-direction: column; min-width: 0; }
  .inv-meta .nm { font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
