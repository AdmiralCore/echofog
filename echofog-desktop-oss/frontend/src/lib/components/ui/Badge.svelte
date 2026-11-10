<script>
    /**
     * A small label. variant: neutral | accent | ok | warn | danger | info.
     * kind adds meaning on top: "live" (the red LIVE mark), "quality" (stream quality such as 1440p60,
     * tabular digits), "direct" and "relay" (how a call is connected; the text defaults to the
     * translated "Direct" / "Via relay").
     */
    import { t } from '../../i18n/index.js';
    import Icon from './Icon.svelte';

    export let variant = 'neutral';
    export let kind = '';
    export let icon = '';
    export let dot = false;
    export let size = 'md'; // sm | md

    $: ic = icon || (kind === 'direct' ? 'check' : kind === 'relay' ? 'relay' : '');
    $: tone = kind === 'live' ? 'live' : kind === 'direct' ? 'ok' : kind === 'relay' ? 'warn' : variant;
</script>

<span class="ui-badge {tone} {size}" class:mono={kind === 'quality'} class:live={kind === 'live'}>
    {#if kind === 'live'}<span class="pulse" aria-hidden="true"></span>{:else if dot}<span class="dot" aria-hidden="true"></span>{/if}
    {#if ic}<Icon name={ic} size={12} stroke={2} />{/if}
    <slot>
        {#if kind === 'live'}{$t('ui.live')}{:else if kind === 'direct'}{$t('ui.direct')}{:else if kind === 'relay'}{$t('ui.relay')}{/if}
    </slot>
</span>

<style>
    .ui-badge {
        display: inline-flex; align-items: center; gap: 4px; height: 20px; padding: 0 7px;
        border-radius: var(--r-sm); font-size: var(--fs-2xs); line-height: 1; font-weight: var(--fw-semibold);
        background: var(--badge-bg); color: var(--badge-text); white-space: nowrap; letter-spacing: 0.01em;
    }
    .sm { height: 16px; padding: 0 5px; }
    .mono { font-variant-numeric: tabular-nums; font-feature-settings: "tnum", "zero"; }
    .accent { background: var(--accent-soft); color: var(--accent-fg); }
    .ok { background: var(--ok-soft); color: var(--ok-fg); }
    .warn { background: var(--warn-soft); color: var(--warn-fg); }
    .danger { background: var(--danger-soft); color: var(--danger-fg); }
    .info { background: var(--info-soft); color: var(--info); }
    .live { background: var(--live); color: var(--live-ink); letter-spacing: 0.06em; font-weight: var(--fw-bold); }
    .dot, .pulse { width: 6px; height: 6px; border-radius: 50%; background: currentColor; flex: none; }
    .pulse { background: var(--live-ink); animation: live-pulse 1.8s ease-in-out infinite; }
    @keyframes live-pulse { 50% { opacity: 0.35; } }
</style>
