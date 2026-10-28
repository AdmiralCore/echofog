<script>
  import { createEventDispatcher, tick } from 'svelte';
  import { slide } from 'svelte/transition';
  import { preview } from '../../stores/chats.js';
  import { t } from '../../i18n/index.js';
  import Icon from '../../components/ui/Icon.svelte';

  /** Where a message or a comment is written: the text, the answer or edit under way, a file. */
  export let text = '';
  export let replyTo = null; // the message answered
  export let editing = null; // my message whose text is changing
  export let replyName = ''; // who is answered
  export let placeholder = '';
  export let files = false; // offer to attach a file
  export let thread = false; // the composer of a thread panel
  export let busy = false;

  const dispatch = createEventDispatcher();
  let input;

  /** Puts the cursor in the field. */
  export async function focus() {
    await tick();
    grow();
    input?.focus();
  }

  // the field grows with its text up to a few lines
  export function grow() {
    if (!input) return;
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 160) + 'px';
  }
  $: text, tick().then(grow);

  function submit() {
    if (!text.trim() || busy) return;
    dispatch('submit');
  }

  function keydown(e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      dispatch(editing || replyTo ? 'cancel' : 'escape');
    } else if (e.key === 'ArrowUp' && !text) {
      // the last message of mine opens for editing, as in other messengers
      dispatch('editlast', e);
    }
  }
</script>

<form class="composer" class:thread-composer={thread} on:submit|preventDefault={submit}>
  {#if replyTo || editing}
    <div class="draft-ctx" transition:slide={{ duration: 150 }}>
      <Icon name={editing ? 'edit' : 'reply'} size={16} />
      <button type="button" class="grow ctx-body" on:click={() => dispatch('goto', (editing || replyTo).id)}>
        <b>{editing ? $t('chats.editing') : $t('chats.replyingTo', { name: replyName })}</b>
        <span>{preview(editing || replyTo)}</span>
      </button>
      <button type="button" class="icon tiny" title={$t('common.cancel')} aria-label={$t('common.cancel')} on:click={() => dispatch('cancel')}><Icon name="close" size={14} stroke={2.2} /></button>
    </div>
  {/if}
  <div class="field" class:withattach={files && !editing}>
    {#if files && !editing}
      <button type="button" class="attach" title={$t('files.attach')} aria-label={$t('files.attach')} on:click={() => dispatch('attach')}><Icon name="paperclip" size={18} /></button>
    {/if}
    <textarea bind:this={input} rows="1" {placeholder} bind:value={text} on:keydown={keydown} on:input={grow} />
    <button class="send" class:ready={text.trim()} type="submit" title={$t('chats.send')} aria-label={$t('chats.send')} data-silent disabled={busy || !text.trim()}><Icon name="send" size={17} stroke={2} /></button>
  </div>
</form>
