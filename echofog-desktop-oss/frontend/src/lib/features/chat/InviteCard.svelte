<script>
  /** A group invitation posted in a chat, as a card: the group's name (signed by its creator, read
   *  from the invitation alone, nothing fetched), how many it names and its term. Joining is not
   *  done here: the button opens the usual "Open a link" window, which asks first. */
  import { App } from '../../api.js';
  import { openLinkDialog } from '../../links.js';
  import { chats, openChat } from '../../stores/chats.js';
  import { t, intlLocale } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';

  export let link; // the invitation, as fromWebLink gives it
  export let mine = false;

  let info = null;
  $: read(link);
  async function read(l) {
    info = null;
    if (!l) return;
    try {
      const p = await App.InspectGroupInvite(l);
      if (l === link) info = p;
    } catch (_) { /* not one we can read: the text stays a plain link */ }
  }
  $: joined = info && (info.joined || $chats.some((c) => c.groupId === info.groupId));
  $: when = info?.exp ? new Date(info.exp * 1000).toLocaleDateString(intlLocale(), { day: '2-digit', month: '2-digit' }) : '';
</script>

{#if info}
  <div class="inv" class:mine>
    <div class="top">
      <Avatar id={'g:' + info.groupId} name={info.title} size={42} />
      <div class="what">
        <span class="eyebrow">{$t('chats.invCard.' + info.kind)}</span>
        <b class="title" title={info.title}>{info.title}</b>
        <span class="sub">
          {#if info.members}{$t('chats.members', { n: info.members })}{/if}
          {#if info.approval}{info.members ? ' · ' : ''}{$t('chats.inv.withApproval')}{/if}
          {#if info.expired}{' · '}{$t('chats.invCard.expired')}{:else if when}{' · '}{$t('chats.invCard.until', { when })}{/if}
        </span>
      </div>
    </div>
    {#if joined}
      <button class="small with-icon" on:click={() => openChat('g:' + info.groupId)}><Icon name="message" size={14} /> {$t('chats.invCard.open')}</button>
    {:else if !mine}
      <button class="primary small with-icon" disabled={info.expired} on:click={() => openLinkDialog(link)}><Icon name="users" size={14} /> {$t('chats.invCard.join')}</button>
    {/if}
  </div>
{/if}

<style>
  .inv {
    display: flex; flex-direction: column; gap: 10px; margin-top: 8px; padding: 12px; min-width: min(260px, 60vw); max-width: 340px;
    border-radius: 14px; background: var(--raised); border: 1px solid var(--line-soft);
  }
  .inv.mine { background: rgba(255, 255, 255, 0.14); border-color: rgba(255, 255, 255, 0.25); }
  .top { display: flex; gap: 10px; align-items: center; }
  .what { min-width: 0; display: flex; flex-direction: column; gap: 1px; }
  .eyebrow { font-size: 11px; font-weight: 650; letter-spacing: 0.03em; text-transform: uppercase; color: var(--accent); }
  .mine .eyebrow { color: rgba(255, 255, 255, 0.85); }
  .title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sub { font-size: 12px; color: var(--muted); }
  .mine .sub { color: rgba(255, 255, 255, 0.78); }
  .inv button { align-self: flex-start; }
  .mine button { color: #fff; border-color: rgba(255, 255, 255, 0.4); background: rgba(255, 255, 255, 0.12); }
</style>
