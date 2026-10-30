<script>
  import { App, errText, copy } from '../../api.js';
  import { webLinkOf } from '../../links.js';
  import { t, intlLocale } from '../../i18n/index.js';
  import { ask } from '../../stores/dialog.js';
  import { membersVersion, chats } from '../../stores/chats.js';
  import { hexColor } from '../../perms.js';
  import Icon from '../../components/ui/Icon.svelte';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Modal from '../../components/ui/Modal.svelte';
  import Pills from '../../components/ui/Pills.svelte';

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
  $: roleItems = [{ value: '', label: $t('chats.inv.defaultRole') }, ...givable.map((r) => ({ value: r.id, label: roleName(r.id), color: hexColor(r.color) || 'var(--faint)' }))];
  $: termItems = TERMS.map((h) => ({ value: h, label: termLabel(h) }));
  $: useItems = USES.map((u) => ({ value: u, label: u ? String(u) : '∞', hint: u ? '' : $t('chats.inv.unlimited') }));

  // contacts invited in their direct chats: each gets an invitation of their own, which their app
  // shows as a card with a button to join
  let inviting = null; // { q, picked: Set, role, sent, busy }
  $: contacts = $chats.filter((c) => c.kind === 'direct' && c.peerId && !c.request && !c.blocked);
  $: shownContacts = inviting ? contacts.filter((c) => !inviting.q.trim() || c.name.toLowerCase().includes(inviting.q.trim().toLowerCase())) : [];
  function togglePick(id) {
    const s = new Set(inviting.picked);
    s.has(id) ? s.delete(id) : s.add(id);
    inviting = { ...inviting, picked: s };
  }
  async function sendInvites() {
    inviting = { ...inviting, busy: true };
    error = '';
    try {
      const n = await App.GroupInviteContacts(conv.groupId, [...inviting.picked], inviting.role);
      inviting = { ...inviting, busy: false, sent: n, picked: new Set() };
      await load(conv.groupId);
    } catch (err) {
      inviting = { ...inviting, busy: false };
      error = errText(err);
    }
  }

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
    {#if conv.can.invite && !making && contacts.length}
      <button class="invite-contacts with-icon" on:click={() => (inviting = { q: '', picked: new Set(), role: '', sent: 0, busy: false })}>
        <Icon name="userPlus" size={16} /> <span class="grow">{$t('chats.invc.button')}</span><Icon name="chevronRight" size={14} />
      </button>
    {/if}
    {#if quick && !making}
      <div class="copybox">
        <span class="mono grow clip">{quickWeb || quick}</span>
        <button class="icon" title={$t('links.copyWeb')} on:click={() => doCopy('quick', quickWeb || quick)}><Icon name={copied === 'quick' ? 'check' : 'copy'} size={16} /></button>
      </div>
      <p class="small faint">{$t(list ? 'chats.inv.quickHint' : 'chats.inviteHint')}</p>
      {#if quickWeb}<button class="link-btn small" on:click={() => doCopy('quickApp', quick)}>{copied === 'quickApp' ? $t('chats.inv.copied') : $t('chats.inv.copyApp')}</button>{/if}
    {/if}

    {#if making}
      <form class="inv-make card" on:submit|preventDefault={create}>
        <label class="field-label" for="inv-label">{$t('chats.inv.label')}</label>
        <input id="inv-label" bind:value={making.label} maxlength="64" placeholder={$t('chats.inv.labelHint')} />
        <span class="field-label">{$t('chats.inv.term')}</span>
        <Pills items={termItems} bind:value={making.hours} label={$t('chats.inv.term')} />
        <span class="field-label">{$t('chats.inv.uses')}</span>
        <Pills items={useItems} bind:value={making.uses} label={$t('chats.inv.uses')} />
        {#if givable.length}
          <span class="field-label">{$t('chats.inv.role')}</span>
          <Pills items={roleItems} bind:value={making.role} label={$t('chats.inv.role')} />
        {/if}
        <label class="toggle">
          <input type="checkbox" bind:checked={making.approval} />
          <span class="knob" aria-hidden="true"></span>
          <span class="small">{$t('chats.inv.approval')}</span>
        </label>
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

{#if inviting}
  <Modal width={440} label={$t('chats.invc.title')} on:close={() => (inviting = null)}>
    <div class="invc">
      <div class="invc-head">
        <h3 class="grow">{$t('chats.invc.title')}</h3>
        <button class="icon tiny" aria-label={$t('common.close')} on:click={() => (inviting = null)}><Icon name="close" size={15} /></button>
      </div>
      <p class="small muted">{$t('chats.invc.hint', { name: conv.name })}</p>
      <!-- svelte-ignore a11y-autofocus -->
      <input bind:value={inviting.q} placeholder={$t('chats.invc.search')} autofocus />
      <div class="invc-list">
        {#each shownContacts as c (c.peerId)}
          <button class="invc-row" class:on={inviting.picked.has(c.peerId)} on:click={() => togglePick(c.peerId)}>
            <Avatar id={c.peerId} name={c.name} src={c.avatar} size={34} />
            <span class="grow nm">{c.name}</span>
            <span class="tick" aria-hidden="true">{#if inviting.picked.has(c.peerId)}<Icon name="check" size={13} stroke={3} />{/if}</span>
          </button>
        {:else}
          <p class="small faint">{$t('chats.invc.none')}</p>
        {/each}
      </div>
      {#if givable.length}
        <span class="field-label">{$t('chats.inv.role')}</span>
        <Pills items={roleItems} bind:value={inviting.role} label={$t('chats.inv.role')} />
      {/if}
      {#if inviting.sent}<p class="small ok"><Icon name="check" size={14} stroke={2.4} /> {$t('chats.invc.sent', { n: inviting.sent })}</p>{/if}
      {#if error}<p class="error small">{error}</p>{/if}
      <div class="row inv-actions">
        <span class="grow small faint">{$t('chats.invc.terms')}</span>
        <button class="primary" disabled={inviting.busy || !inviting.picked.size} on:click={sendInvites}>
          {#if inviting.busy}<span class="spinner light"></span>{/if}{$t('chats.invc.send', { n: inviting.picked.size })}
        </button>
      </div>
    </div>
  </Modal>
{/if}

<style>
  .inv-head { align-items: center; gap: 6px; }
  .link-btn { background: none; border: 0; padding: 2px 0; color: var(--accent); cursor: pointer; text-align: left; }
  .inv-make { display: flex; flex-direction: column; gap: 6px; margin-top: 8px; }
  .inv-make.card { gap: 8px; padding: 14px; border-radius: 14px; background: var(--raised); border: 1px solid var(--line-soft); }
  .field-label { font-size: 11.5px; font-weight: 650; letter-spacing: 0.03em; text-transform: uppercase; color: var(--faint); margin-top: 4px; }
  .toggle { display: flex; align-items: center; gap: 10px; margin-top: 6px; cursor: pointer; }
  .toggle input { position: absolute; opacity: 0; width: 0; height: 0; }
  .toggle .knob { position: relative; flex: none; width: 34px; height: 20px; border-radius: 99px; background: var(--line); transition: background var(--dur, 0.15s); }
  .toggle .knob::after { content: ''; position: absolute; top: 2px; left: 2px; width: 16px; height: 16px; border-radius: 50%; background: #fff; transition: transform var(--dur, 0.15s); box-shadow: 0 1px 3px rgba(0, 0, 0, 0.3); }
  .toggle input:checked + .knob { background: var(--accent); }
  .toggle input:checked + .knob::after { transform: translateX(14px); }
  .toggle input:focus-visible + .knob { outline: 2px solid var(--accent); outline-offset: 2px; }
  .invite-contacts {
    display: flex; align-items: center; gap: 10px; width: 100%; height: 42px; padding: 0 12px; margin-top: 2px;
    border-radius: 12px; background: var(--accent-soft); color: var(--accent); border: 1px solid transparent; font-weight: 600;
  }
  .invite-contacts:hover:not(:disabled) { background: var(--accent-soft); border-color: var(--accent); }
  .invc { display: flex; flex-direction: column; gap: 10px; padding: 20px; }
  .invc-head { display: flex; align-items: center; }
  .invc-head h3 { margin: 0; font-size: 16px; }
  .invc-list { display: flex; flex-direction: column; gap: 2px; max-height: 300px; overflow-y: auto; margin: 0 -6px; }
  .invc-row { display: flex; align-items: center; gap: 10px; padding: 6px; border-radius: 12px; background: transparent; border: none; text-align: left; color: var(--text); }
  .invc-row:hover:not(:disabled) { background: var(--hover); border: none; }
  .invc-row .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 560; }
  .tick { width: 22px; height: 22px; flex: none; border-radius: 50%; display: grid; place-items: center; border: 2px solid var(--line); color: #fff; }
  .invc-row.on .tick { background: var(--accent); border-color: var(--accent); }
  .ok { color: var(--ok); display: flex; align-items: center; gap: 6px; margin: 0; }
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
