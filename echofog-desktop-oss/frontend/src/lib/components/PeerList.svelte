<script>
  // An editable list of peers (allowed on a private relay, free on a shared exit): paste a peer ID
  // or contact link, or pick a contact.
  import { createEventDispatcher } from 'svelte';
  import { peerIdFrom } from '../contact.js';
  import { short } from '../format.js';
  import Icon from './ui/Icon.svelte';
  import { t } from '../i18n/index.js';

  export let ids = [];
  export let contacts = [];
  export let disabled = false;
  export let empty = '';
  export let label = '';

  const dispatch = createEventDispatcher();
  let input = '';
  let error = '';

  $: names = Object.fromEntries(contacts.map((c) => [(c.peer_id || '').toLowerCase(), c.alias]));
  $: pickable = contacts.filter((c) => c.peer_id && !ids.includes(c.peer_id.toLowerCase()));

  function set(next) {
    error = '';
    dispatch('change', next);
  }

  async function add() {
    error = '';
    const id = await peerIdFrom(input, contacts);
    if (!id) {
      error = $t('relay.peers.bad');
      return;
    }
    input = '';
    if (!ids.includes(id)) set([...ids, id]);
  }

  function pick(e) {
    const id = e.target.value;
    e.target.value = '';
    if (id && !ids.includes(id)) set([...ids, id]);
  }
</script>

<div class="plist">
  <div class="chips" aria-label={label || $t('relay.peers.label')}>
    {#each ids as id (id)}
      <span class="chip peer" title={id}>
        {#if names[id]}{names[id]} <span class="faint mono">{short(id)}</span>{:else}<span class="mono">{short(id)}</span>{/if}
        <button class="x" title={$t('common.remove')} aria-label={$t('relay.peers.removePeer', { name: names[id] || short(id) })} {disabled} on:click={() => set(ids.filter((x) => x !== id))}><Icon name="close" size={12} /></button>
      </span>
    {:else}
      <span class="small faint">{empty || $t('relay.peers.empty')}</span>
    {/each}
  </div>
  <div class="row add">
    <input class="grow" bind:value={input} placeholder={$t('relay.peers.placeholder')} {disabled} on:keydown={(e) => e.key === 'Enter' && add()} />
    <button on:click={add} disabled={disabled || !input.trim()}>{$t('common.add')}</button>
    {#if pickable.length}
      <select on:change={pick} {disabled} aria-label={$t('relay.peers.addContact')}>
        <option value="">{$t('relay.peers.fromContacts')}</option>
        {#each pickable as c (c.peer_id)}<option value={c.peer_id.toLowerCase()}>{c.alias || short(c.peer_id)}</option>{/each}
      </select>
    {/if}
  </div>
  {#if error}<p class="small error">{error}</p>{/if}
</div>

<style>
  .plist { display: flex; flex-direction: column; gap: 8px; }
  .chips { display: flex; flex-wrap: wrap; gap: 6px; min-height: 26px; align-items: center; }
  .peer { gap: 6px; padding-right: 4px; }
  .peer .mono { font-size: 11px; }
  .x { width: 20px; height: 20px; padding: 0; border-radius: 50%; display: grid; place-items: center; background: transparent; border: none; color: inherit; opacity: 0.7; }
  .x:hover:not(:disabled) { opacity: 1; background: var(--hover); border: none; }
  .add { gap: 8px; flex-wrap: wrap; }
  .add input { min-width: 200px; }
  select { width: auto; max-width: 200px; }
</style>
