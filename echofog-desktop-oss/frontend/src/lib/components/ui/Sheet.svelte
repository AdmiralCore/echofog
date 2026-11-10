<script>
    /**
     * A dialog with a title bar (title, optional hint, close button), a scrolling body and an optional
     * footer for the buttons. Built on Modal: Escape or a click outside closes it. Emits `close`.
     */
    import { createEventDispatcher } from 'svelte';
    import Modal from './Modal.svelte';
    import IconButton from './IconButton.svelte';
    import { t } from '../../i18n/index.js';

    export let title = '';
    export let hint = '';
    export let width = 640;

    const dispatch = createEventDispatcher();
    const close = () => dispatch('close');
</script>

<Modal label={title} {width} on:close={close}>
    <header>
        <div class="grow">
            <h2>{title}</h2>
            {#if hint}<p class="small muted">{hint}</p>{/if}
        </div>
        <IconButton icon="close" label={$t('common.close')} on:click={close} />
    </header>
    <div class="body"><slot /></div>
    {#if $$slots.footer}<footer><slot name="footer" /></footer>{/if}
</Modal>

<style>
    header { display: flex; gap: var(--sp-3); align-items: flex-start; padding: var(--sp-4) var(--sp-5) var(--sp-3); }
    header h2 { font-size: var(--fs-lg); line-height: var(--lh-lg); }
    header p { margin-top: 2px; line-height: 1.45; }
    .body { display: flex; flex-direction: column; gap: var(--sp-3); padding: 0 var(--sp-5) var(--sp-4); min-height: 0; overflow: auto; }
    footer { display: flex; gap: var(--sp-2); flex-wrap: wrap; align-items: center; padding: var(--sp-3) var(--sp-5); border-top: 1px solid var(--border); background: var(--surface-2); }
</style>
