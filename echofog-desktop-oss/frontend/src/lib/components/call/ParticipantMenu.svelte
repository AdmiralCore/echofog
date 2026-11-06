<script>
    import { createEventDispatcher, onMount } from 'svelte';
    import { callService, callState } from '../../services/call.service.js';
    import { peerName } from '../../stores/peers.store.js';
    import Icon from '../ui/Icon.svelte';
    import Avatar from '../ui/Avatar.svelte';
    import Badge from '../ui/Badge.svelte';
    import BackdropPicker from './BackdropPicker.svelte';
    import { t } from '../../i18n/index.js';

    export let peer;
    export let x = 0;
    export let y = 0;

    const dispatch = createEventDispatcher();
    let menu;
    let confirmReport = false;
    let copied = false;

    $: isSelf = peer === $callState.self;
    $: isHost = peer === $callState.coordinator;
    $: volume = Math.round(($callState.peerVolume[peer] ?? 1) * 100);
    $: muted = !!$callState.localMuted[peer];
    $: reported = !!$callState.reported[peer];

    onMount(() => {
        // keep the popover inside the window
        const r = menu.getBoundingClientRect();
        if (r.right > window.innerWidth - 8) x = window.innerWidth - r.width - 8;
        if (r.bottom > window.innerHeight - 8) y = window.innerHeight - r.height - 8;
    });

    function close() {
        dispatch('close');
    }

    async function copyId() {
        try {
            await navigator.clipboard.writeText(peer);
            copied = true;
            setTimeout(() => (copied = false), 1500);
        } catch (_) { /* clipboard blocked */ }
    }

    async function report() {
        if (!confirmReport) {
            confirmReport = true;
            return;
        }
        await callService.reportPeer(peer, 'spam or garbage media');
        close();
    }
</script>

<svelte:window on:keydown={(e) => e.key === 'Escape' && close()} />

<!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
<div class="scrim" on:click={close} on:contextmenu|preventDefault={close}></div>
<div
    bind:this={menu}
    class="pmenu"
    class:self={isSelf}
    style="left:{x}px;top:{y}px"
    role="menu"
    aria-label={$peerName(peer)}
>
    <div class="who">
        <Avatar id={peer} name={$peerName(peer)} size={36} speaking={!!$callState.speaking?.[peer]} />
        <div class="who-text">
            <p class="nm">{$peerName(peer)}{#if isHost}<Badge variant="accent" size="sm">{$t('call.host')}</Badge>{/if}</p>
            <p class="id">{peer.slice(0, 16)}…</p>
        </div>
    </div>

    {#if !isSelf}
        <div class="vol">
            <div class="vol-head">
                <span class="eyebrow">{$t('call.userVolume')}</span>
                <span class="pct">{volume}%</span>
            </div>
            <input
                type="range"
                min="0"
                max="200"
                step="5"
                value={volume}
                class="range"
                aria-label={$t('call.userVolume')}
                on:input={(e) => callService.setPeerVolume(peer, Number(e.currentTarget.value) / 100)}
            />
        </div>
        <div class="sep"></div>
        <button class="menu-row" role="menuitem" on:click={() => callService.toggleLocalMute(peer)}>
            <Icon name={muted ? 'volume' : 'volumeOff'} size={17} />
            <span class="grow">{muted ? $t('call.unmuteForMe') : $t('call.muteForMe')}</span>
            {#if muted}<Icon name="check" size={16} />{/if}
        </button>
        <button class="menu-row bad" role="menuitem" disabled={reported} on:click={report}>
            <Icon name="flag" size={17} />
            <span class="grow">
                {#if reported}{$t('call.reported')}{:else if confirmReport}{$t('call.reportConfirm')}{:else}{$t('call.reportSpam')}{/if}
            </span>
        </button>
        {#if confirmReport && !reported}
            <p class="note">{$t('call.reportNote')}</p>
        {/if}
        <div class="sep"></div>
    {:else}
        <div class="bg">
            <p class="eyebrow">{$t('call.bg.title')}</p>
            <BackdropPicker {peer} compact />
            <p class="note flat">{$t('call.bg.hint')}</p>
        </div>
        <div class="sep"></div>
    {/if}
    <button class="menu-row" role="menuitem" on:click={copyId}>
        <Icon name={copied ? 'check' : 'copy'} size={17} />
        <span class="grow">{copied ? $t('common.copied') : $t('call.copyId')}</span>
    </button>
</div>

<style>
    .scrim { position: fixed; inset: 0; z-index: var(--z-menu, 800); }
    .pmenu {
        position: fixed; box-sizing: border-box; z-index: calc(var(--z-menu, 800) + 1); width: 264px; padding: 6px; border-radius: var(--r-menu, 10px);
        background: var(--surface-2); color: var(--text); border: 1px solid var(--border-strong); box-shadow: var(--shadow-lg);
        font-size: var(--fs-md); animation: pm-in var(--dur) var(--ease); transform-origin: top left;
    }
    .pmenu.self { width: 288px; }
    @keyframes pm-in { from { opacity: 0; transform: scale(0.96); } to { opacity: 1; transform: none; } }
    @media (prefers-reduced-motion: reduce) { .pmenu { animation: none; } }
    .who { display: flex; align-items: center; gap: var(--sp-3); padding: 6px 8px 8px; }
    .who-text { min-width: 0; }
    .nm { display: flex; align-items: center; gap: 6px; font-weight: var(--fw-semibold); color: var(--text); }
    .nm :global(.ui-badge) { flex: none; }
    .id { font-family: ui-monospace, SFMono-Regular, Consolas, monospace; font-size: var(--fs-2xs); color: var(--faint); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .vol, .bg { padding: 4px 8px 8px; }
    .vol-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 6px; }
    .pct { font-size: var(--fs-xs); color: var(--muted); font-variant-numeric: tabular-nums; }
    .range { width: 100%; accent-color: var(--accent); }
    .bg .eyebrow { margin-bottom: 6px; }
    .sep { height: 1px; background: var(--border); margin: 4px 2px; }
    .note { padding: 0 10px 6px; font-size: var(--fs-xs); line-height: 1.4; color: var(--muted); }
    .note.flat { padding: 0; margin-top: 6px; color: var(--faint); }
    .menu-row {
        display: flex; align-items: center; gap: 10px; width: 100%; padding: 7px 10px; border: none; border-radius: var(--r-md);
        background: transparent; color: var(--text); font-weight: var(--fw-medium); text-align: left;
    }
    .menu-row:hover:not(:disabled) { background: var(--hover); border: none; }
    .menu-row:focus-visible { outline: 2px solid var(--accent-ring); outline-offset: -2px; }
    .menu-row.bad { color: var(--danger-fg); }
    .menu-row.bad:hover:not(:disabled) { background: var(--danger-soft); }
    .menu-row:disabled { opacity: 0.5; cursor: default; }
    .grow { flex: 1; min-width: 0; }
</style>
