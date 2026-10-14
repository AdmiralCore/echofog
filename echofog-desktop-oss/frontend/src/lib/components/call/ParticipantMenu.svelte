<script>
    import { createEventDispatcher, onMount } from 'svelte';
    import { callService, callState } from '../../services/call.service.js';
    import { peerName } from '../../stores/peers.store.js';
    import Icon from '../ui/Icon.svelte';
    import Avatar from '../ui/Avatar.svelte';

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
    class="fixed z-[201] w-64 rounded-xl bg-[#111214] border border-white/10 shadow-2xl p-1.5 text-[14px] text-[#dbdee1]"
    style="left:{x}px;top:{y}px"
    role="menu"
>
    <div class="flex items-center gap-2.5 px-2 py-2">
        <Avatar id={peer} name={$peerName(peer)} size={32} />
        <div class="min-w-0">
            <p class="font-semibold text-white truncate">{$peerName(peer)}</p>
            <p class="text-[11px] text-[#949ba4] font-mono truncate">{peer.slice(0, 16)}…</p>
        </div>
    </div>

    {#if !isSelf}
        <div class="px-2 pt-1 pb-2">
            <div class="flex items-center justify-between text-[12px] text-[#b5bac1] mb-1.5">
                <span class="font-semibold uppercase tracking-wide">User volume</span>
                <span>{volume}%</span>
            </div>
            <input
                type="range"
                min="0"
                max="200"
                step="5"
                value={volume}
                class="w-full accent-[var(--color-accent)]"
                aria-label="User volume"
                on:input={(e) => callService.setPeerVolume(peer, Number(e.currentTarget.value) / 100)}
            />
        </div>
        <div class="h-px bg-white/10 my-1"></div>
        <button class="menu-row" role="menuitem" on:click={() => callService.toggleLocalMute(peer)}>
            <Icon name={muted ? 'volume' : 'volumeOff'} size={18} />
            <span class="flex-1 text-left">{muted ? 'Unmute for me' : 'Mute for me'}</span>
            {#if muted}<Icon name="check" size={16} className="text-[var(--color-accent)]" />{/if}
        </button>
        <button class="menu-row !text-[#f23f43] hover:!bg-[#f23f43] hover:!text-white" role="menuitem" disabled={reported} on:click={report}>
            <Icon name="flag" size={18} />
            <span class="flex-1 text-left">
                {#if reported}Reported{:else if confirmReport}Click again to report{:else}Report spam{/if}
            </span>
        </button>
        {#if confirmReport && !reported}
            <p class="px-2.5 pb-1.5 text-[12px] text-[#949ba4] leading-snug">
                They are muted for you right away. The room removes them when a third of members report them too.
            </p>
        {/if}
        <div class="h-px bg-white/10 my-1"></div>
    {/if}
    <button class="menu-row" role="menuitem" on:click={copyId}>
        <Icon name="copy" size={18} />
        <span class="flex-1 text-left">{copied ? 'Copied' : 'Copy user ID'}</span>
    </button>
</div>

<style>
    .menu-row {
        display: flex;
        align-items: center;
        gap: 10px;
        width: 100%;
        padding: 7px 10px;
        border-radius: 6px;
        color: #dbdee1;
    }
    .menu-row:hover:not(:disabled) {
        background: var(--color-accent);
        color: #fff;
    }
    .menu-row:disabled {
        opacity: 0.5;
        cursor: default;
    }
</style>
