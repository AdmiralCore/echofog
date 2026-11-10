<script>
    /** Mount once (App.svelte): shows what stores/toast.js is asked to say, bottom right, newest last. */
    import { fly } from 'svelte/transition';
    import { toasts, dismissToast } from '../../stores/toast.js';
    import { t } from '../../i18n/index.js';
    import Icon from './Icon.svelte';

    const icons = { info: 'info', success: 'success', warning: 'warning', error: 'error' };
</script>

<div class="stack" role="region" aria-label={$t('ui.notification')}>
    {#each $toasts as n (n.id)}
        <div
            class="toast {n.kind}"
            role={n.kind === 'error' || n.kind === 'warning' ? 'alert' : 'status'}
            transition:fly={{ x: 24, duration: 160 }}
        >
            <span class="ic"><Icon name={icons[n.kind] || 'info'} size={18} /></span>
            <p>{n.message}</p>
            {#if n.action}
                <button type="button" class="act" on:click={() => { n.action.run?.(); dismissToast(n.id); }}>{n.action.label}</button>
            {/if}
            <button type="button" class="x" aria-label={$t('ui.dismiss')} on:click={() => dismissToast(n.id)}><Icon name="close" size={14} /></button>
        </div>
    {/each}
</div>

<style>
    .stack {
        position: fixed; right: var(--sp-4); bottom: var(--sp-4); z-index: var(--z-toast);
        display: flex; flex-direction: column; gap: var(--sp-2); width: min(380px, calc(100vw - 32px));
        pointer-events: none;
    }
    .toast {
        pointer-events: auto; display: flex; align-items: flex-start; gap: 10px; padding: 10px 10px 10px 12px;
        border-radius: var(--r-lg); background: var(--surface-3); border: 1px solid var(--border-strong);
        box-shadow: var(--shadow-lg); color: var(--text);
    }
    .ic { display: inline-flex; margin-top: 1px; color: var(--info); }
    .success .ic { color: var(--ok); }
    .warning .ic { color: var(--warn); }
    .error .ic { color: var(--danger); }
    p { flex: 1; margin: 0; min-width: 0; font-size: var(--fs-md); line-height: var(--lh-md); padding-top: 1px; overflow-wrap: anywhere; }
    .act {
        height: var(--ctl-sm); padding: 0 8px; border: 0; background: transparent; border-radius: var(--r-md);
        color: var(--accent-fg); font-size: var(--fs-sm); font-weight: var(--fw-semibold);
    }
    .act:hover:not(:disabled) { background: var(--accent-soft); border: 0; }
    .x {
        width: 24px; height: 24px; padding: 0; display: inline-flex; align-items: center; justify-content: center;
        border: 0; background: transparent; color: var(--muted); border-radius: var(--r-sm);
    }
    .x:hover:not(:disabled) { background: var(--hover); color: var(--text); border: 0; }
</style>
