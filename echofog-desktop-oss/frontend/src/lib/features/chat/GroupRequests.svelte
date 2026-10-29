<script>
  import { App, errText } from '../../api.js';
  import { membersVersion } from '../../stores/chats.js';
  import { t } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import { useChat } from './context.js';

  /** Requests to join a group (by an invitation that asks for approval), for whoever may let
   *  members in: accept lets the newcomer in, decline tells them. Shown only while some wait. */
  export let conv;

  const { nameOf } = useChat();
  let list = [];
  let busy = '';
  let error = '';

  $: void $membersVersion, load(conv.groupId);
  async function load(id) {
    try { list = (await App.GroupJoinRequests(id)) || []; } catch (_) { list = []; }
  }
  async function decide(r, accept) {
    busy = r.peerId;
    error = '';
    try {
      await App.GroupJoinDecide(conv.groupId, r.peerId, accept);
      await load(conv.groupId);
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }
</script>

{#if list.length}
  <div class="block">
    <div class="eyebrow">{$t('chats.req.title', { n: list.length })}</div>
    {#each list as r (r.peerId)}
      <div class="req">
        <Avatar id={r.peerId} name={$nameOf(r.peerId)} size={28} />
        <div class="grow meta">
          <span class="nm">{$nameOf(r.peerId)}</span>
          {#if r.invite}<span class="small faint">{$t('chats.req.via', { label: r.invite })}</span>{/if}
        </div>
        <button class="ghost small" disabled={busy === r.peerId} on:click={() => decide(r, false)}>{$t('chats.req.decline')}</button>
        <button class="primary small" disabled={busy === r.peerId} on:click={() => decide(r, true)}>{$t('chats.req.accept')}</button>
      </div>
    {/each}
    {#if error}<p class="error small">{error}</p>{/if}
  </div>
{/if}

<style>
  .req { display: flex; align-items: center; gap: 8px; min-height: 40px; }
  .meta { display: flex; flex-direction: column; min-width: 0; }
  .meta .nm { font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
