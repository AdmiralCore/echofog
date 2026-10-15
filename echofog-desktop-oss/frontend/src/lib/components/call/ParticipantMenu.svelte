<script>
    import { createEventDispatcher, onMount } from 'svelte';
    import { callService, callState } from '../../services/call.service.js';
    import { peerName } from '../../stores/peers.store.js';
    import Icon from '../ui/Icon.svelte';
    import Avatar from '../ui/Avatar.svelte';
    import { t } from '../../i18n/index.js';

    export let peer;
    export let x = 0;
    export let y = 0;

    const dispatch = createEventDispatcher();
    let menu;
    let confirmReport = false;
    let copied = false;

    $: isSelf = peer === $callState.self;
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
<div class="fixed inset-0 z-[200]" on:click={close} on:contextmenu|preventDefault={close}></div>
<div
    bind:this={menu}
    class="pmenu fixed z-[201] w-64 rounded-2xl p-1.5 text-[14px]"
    style="left:{x}px;top:{y}px"
    role="menu"
>
    <div class="flex items-center gap-2.5 px-2 py-2">
        <Avatar id={peer} name={$peerName(peer)} size={32} />
        <div class="min-w-0">
            <p class="font-semibold truncate" style="color:var(--text)">{$peerName(peer)}</p>
            <p class="text-[11px] font-mono truncate" style="color:var(--faint)">{peer.slice(0, 16)}…</p>
        </div>
    </div>

    {#if !isSelf}
        <div class="px-2 pt-1 pb-2">
            <div class="flex items-center justify-between text-[12px] mb-1.5" style="color:var(--muted)">
                <span class="font-semibold uppercase tracking-wide">{$t('call.userVolume')}</span>
                <span>{volume}%</span>
            </div>
            <input
                type="range"
                min="0"
                max="200"
                step="5"
                value={volume}
                class="w-full accent-[var(--color-accent)]"
                aria-label={$t('call.userVolume')}
                on:input={(e) => callService.setPeerVolume(peer, Number(e.currentTarget.value) / 100)}
            />
        </div>
        <div class="sep"></div>
        <button class="menu-row" role="menuitem" on:click={() => callService.toggleLocalMute(peer)}>
            <Icon name={muted ? 'volume' : 'volumeOff'} size={18} />
            <span class="flex-1 text-left">{muted ? $t('call.unmuteForMe') : $t('call.muteForMe')}</span>
            {#if muted}<Icon name="check" size={16} className="text-[var(--color-accent)]" />{/if}
        </button>
        <button class="menu-row bad" role="menuitem" disabled={reported} on:click={report}>
            <Icon name="flag" size={18} />
            <span class="flex-1 text-left">
                {#if reported}{$t('call.reported')}{:else if confirmReport}{$t('call.reportConfirm')}{:else}{$t('call.reportSpam')}{/if}
            </span>
        </button>
        {#if confirmReport && !reported}
            <p class="px-2.5 pb-1.5 text-[12px] leading-snug" style="color:var(--muted)">
                {$t('call.reportNote')}
            </p>
        {/if}
        <div class="sep"></div>
    {/if}
    <button class="menu-row" role="menuitem" on:click={copyId}>
        <Icon name="copy" size={18} />
        <span class="flex-1 text-left">{copied ? $t('common.copied') : $t('call.copyId')}</span>
    </button>
</div>

<style>
    .pmenu {
        background: var(--glass);
        backdrop-filter: blur(20px) saturate(1.4);
        color: var(--text);
        border: 1px solid var(--line-soft);
        box-shadow: var(--shadow-lg);
        animation: pm-in 0.28s var(--spring);
        transform-origin: top left;
    }
    @keyframes pm-in { from { opacity: 0; transform: scale(0.94); } to { opacity: 1; transform: none; } }
    .sep { height: 1px; background: var(--line-soft); margin: 4px 0; }
    .menu-row {
        display: flex;
        align-items: center;
        gap: 10px;
        width: 100%;
        padding: 7px 10px;
        border: none;
        border-radius: 9px;
        background: transparent;
        color: var(--text);
        font-weight: 500;
    }
    .menu-row:hover:not(:disabled) { background: var(--accent); color: #fff; border: none; }
    .menu-row.bad { color: var(--danger); }
    .menu-row.bad:hover:not(:disabled) { background: var(--danger); color: #fff; }
    .menu-row:disabled { opacity: 0.5; cursor: default; }
</style>
