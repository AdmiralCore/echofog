<script>
    /**
     * The call stage's note that the call goes through a relay: "via relay · 48 ms", or in a group
     * "2 of 5 via relay". Calm unless the relay costs the call something people notice (a slow
     * round trip, a stream held back). It opens the panel that says why and what helps.
     */
    import { onMount, onDestroy } from 'svelte';
    import { App } from '../../api.js';
    import { callState } from '../../services/call.service.js';
    import { badgeOf } from '../../callpath.js';
    import Icon from '../ui/Icon.svelte';
    import Popover from '../ui/Popover.svelte';
    import CallPathPanel from './CallPathPanel.svelte';
    import { t } from '../../i18n/index.js';

    // each look also pings the relayed links once, which the next look reports
    const EVERY_MS = 3000;

    let path = null;
    let open = false;
    let anchor;
    let timer;

    async function load() {
        if (!inCall) {
            path = null;
            return;
        }
        try {
            path = await App.GetCallPath();
        } catch (_) {
            path = null; // an older core: no badge (the failed call is in the log already)
        }
    }

    // the call state changes with every voice level; only joining and leaving matter here
    $: inCall = $callState.inCall;
    $: inCall, load();
    $: badge = badgeOf(path);
    $: if (!badge) open = false;

    onMount(() => {
        timer = setInterval(load, EVERY_MS);
    });
    onDestroy(() => clearInterval(timer));
</script>

{#if badge}
    <button
        bind:this={anchor}
        class="path-badge shrink-0"
        class:warn={badge.warn}
        class:open
        title={$t(badge.direct ? 'callpath.badgeDirectTitle' : 'callpath.badgeTitle')}
        aria-haspopup="dialog"
        aria-expanded={open}
        on:click={() => (open = !open)}
    >
        <Icon name={badge.direct ? 'check' : 'relay'} size={13} />
        <span>{$t(badge.key, badge.params)}</span>
    </button>
{/if}

{#if open && badge}
    <Popover {anchor} width={380} label={$t('callpath.title')} on:close={() => (open = false)}>
        <CallPathPanel {path} on:changed={load} />
    </Popover>
{/if}

<style>
    .path-badge {
        display: inline-flex;
        align-items: center;
        gap: 5px;
        height: 22px;
        padding: 0 9px;
        border: none;
        border-radius: 999px;
        background: rgba(255, 255, 255, 0.07);
        color: #b5bac1;
        font-size: 12px;
        font-variant-numeric: tabular-nums;
        white-space: nowrap;
    }
    .path-badge:hover:not(:disabled),
    .path-badge.open {
        background: rgba(255, 255, 255, 0.13);
        border: none;
        color: #fff;
    }
    .path-badge.warn { background: rgba(240, 178, 50, 0.14); color: #f0b232; }
    .path-badge.warn:hover:not(:disabled),
    .path-badge.warn.open { background: rgba(240, 178, 50, 0.24); color: #ffc94d; }
</style>
