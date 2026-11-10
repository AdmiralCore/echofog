<script>
  import { createEventDispatcher } from 'svelte';
  import { t } from '../../i18n/index.js';
  import Modal from '../../components/ui/Modal.svelte';
  import Tabs from '../../components/ui/Tabs.svelte';
  import Input from '../../components/ui/Input.svelte';
  import Select from '../../components/ui/Select.svelte';
  import Toggle from '../../components/ui/Toggle.svelte';
  import Button from '../../components/ui/Button.svelte';
  import IconButton from '../../components/ui/IconButton.svelte';
  import ChannelPerms from './ChannelPerms.svelte';

  /** The settings of a channel, a voice room, the main channel or a category, in a window over the app:
   *  General (name, topic, category, slow mode, "only admins write"), Permissions (by role) and Danger
   *  (remove, asked again). `ed` is the edit state ChannelBar keeps (changed in place); save, remove and
   *  move go back to it. */
  export let ed;
  export let cats = []; // the real categories
  export let roles = [];
  export let textChannels = [];
  export let mainName = '';
  export let groupKind = 'group';
  export let canMove = false;

  const dispatch = createEventDispatcher();
  const SLOW = [0, 5, 10, 30, 60, 300, 900, 3600];
  const slowLabel = (s) => (!s ? $t('chats.slowOff') : s < 60 ? $t('chats.slowSec', { n: s }) : s < 3600 ? $t('chats.slowMin', { n: s / 60 }) : $t('chats.slowHour', { n: s / 3600 }));

  $: isCat = !!ed.ch.category;
  $: voice = !!ed.ch.voice;
  $: text = ed.main || (!voice && !isCat);
  $: title = ed.main ? $t('chats.mainSettings') : isCat ? $t('chats.categorySettings') : $t('chats.channelSettings');
  $: shownName = ed.main ? ed.name || mainName : ed.name;
  $: tabs = [
    { id: 'general', label: $t('cset.general') },
    ...(!isCat && groupKind !== 'channel' ? [{ id: 'perms', label: $t('cset.perms') }] : []),
    ...(!ed.main ? [{ id: 'danger', label: $t('cset.danger') }] : []),
  ];
  let tab = 'general';
  $: if (!tabs.some((x) => x.id === tab)) tab = 'general';
  $: slowOptions = SLOW.map((s) => ({ value: s, label: slowLabel(s) }));
  $: catOptions = [{ value: '', label: $t('chats.noCategory') }, ...cats.map((c) => ({ value: c.id, label: c.name }))];
  $: openOptions = [{ value: '', label: mainName }, ...textChannels.map((c) => ({ value: c.id, label: '#' + c.name }))];
  $: canSave = ed.main || !!ed.name.trim();
  const slowSet = (e) => (ed.slowSecs = Number(e.currentTarget.value));
</script>

<Modal width={640} label={title} on:close={() => dispatch('close')}>
  <form class="cs" on:submit|preventDefault={() => canSave && dispatch('save')}>
    <header>
      <div class="grow">
        <h2>{title}</h2>
        {#if shownName}<p class="small muted">{ed.ch.voice ? '' : isCat ? '' : '#'}{shownName}</p>{/if}
      </div>
      <IconButton icon="close" label={$t('common.close')} on:click={() => dispatch('close')} />
    </header>
    <Tabs items={tabs} bind:value={tab} label={title} />

    <div class="body">
      {#if tab === 'general'}
        <Input label={isCat ? $t('chats.categoryName') : $t('chats.channelName')} bind:value={ed.name} maxlength="64" placeholder={ed.main ? $t('chats.mainChannel') : ''} />
        {#if text}
          <Input label={$t('chats.channelTopic')} bind:value={ed.topic} maxlength="256" />
          {#if !isCat}
            <Select label={$t('chats.slowMode')} options={slowOptions} value={ed.slowSecs} on:change={slowSet} />
            <Toggle label={$t('cset.onlyAdmins')} hint={$t('chats.readOnlyHint')} bind:checked={ed.readOnly} />
          {/if}
        {/if}
        {#if !isCat && cats.length}
          <Select label={$t('chats.category')} options={catOptions} bind:value={ed.parent} />
        {/if}
        {#if ed.main}
          <Select label={$t('chats.openOn')} options={openOptions} bind:value={ed.open} />
        {/if}
        {#if canMove}
          <div class="order">
            <span class="small muted grow">{$t('cset.order')}</span>
            <IconButton icon="chevronUp" label={$t('chats.moveUp')} on:click={() => dispatch('move', -1)} />
            <IconButton icon="chevronDown" label={$t('chats.moveDown')} on:click={() => dispatch('move', 1)} />
          </div>
        {/if}
      {:else if tab === 'perms'}
        <ChannelPerms bind:overrides={ed.overrides} {roles} voice={voice} />
      {:else if tab === 'danger'}
        <div class="danger">
          <div class="grow">
            <b>{isCat ? $t('cset.removeCategory') : voice ? $t('cset.removeRoom') : $t('cset.removeChannel')}</b>
            <p class="small muted">{isCat ? $t('cset.removeCategoryHint') : $t('cset.removeChannelHint')}</p>
          </div>
          <Button variant="danger-solid" on:click={() => dispatch('remove')}>{$t('common.remove')}</Button>
        </div>
      {/if}
    </div>

    <footer>
      <Button on:click={() => dispatch('close')}>{$t('common.cancel')}</Button>
      <Button variant="primary" type="submit" disabled={!canSave}>{$t('common.save')}</Button>
    </footer>
  </form>
</Modal>

<style>
  .cs { display: flex; flex-direction: column; max-height: 80vh; min-height: 0; }
  header { display: flex; align-items: center; gap: var(--sp-3); padding: var(--sp-4) var(--sp-4) var(--sp-2) var(--sp-5); }
  header h2 { font-size: var(--fs-lg); line-height: var(--lh-lg); }
  /* the window sits inside the chat screen, whose composer styles a .field of its own */
  .cs :global(.field), .cs :global(.field:focus-within) { display: flex; flex-direction: column; align-items: stretch; gap: 6px; padding: 0; background: none; border: 0; border-radius: 0; box-shadow: none; }
  .cs :global(.tabs) { margin: 0 var(--sp-4); flex: none; }
  .body { flex: 1; min-height: 160px; overflow-y: auto; padding: var(--sp-4) var(--sp-5); display: flex; flex-direction: column; gap: var(--sp-4); }
  footer { display: flex; justify-content: flex-end; gap: var(--sp-2); padding: var(--sp-3) var(--sp-5); border-top: 1px solid var(--border); }
  .order { display: flex; align-items: center; gap: var(--sp-1); padding-top: var(--sp-3); border-top: 1px solid var(--border); }
  .danger { display: flex; align-items: center; gap: var(--sp-4); padding: var(--sp-4); border-radius: var(--r-lg); border: 1px solid var(--danger-soft); background: var(--surface-2); }
  .danger b { font-weight: var(--fw-semibold); }
</style>
