<script>
    import { createEventDispatcher } from 'svelte';
    import { callState } from '../../services/call.service.js';
    import { peerName, peerAvatar } from '../../stores/peers.store.js';
    import Avatar from '../ui/Avatar.svelte';
    import Icon from '../ui/Icon.svelte';
    import VideoView from './VideoView.svelte';

    export let peer;
    /** camera stream of this person, if any */
    export let camera = null;
    export let compact = false;

    const dispatch = createEventDispatcher();

    $: isSelf = peer === $callState.self;
    $: speaking = !!$callState.speaking[peer];
    $: micOff = isSelf && $callState.isMicMuted;
    $: deaf = isSelf && $callState.isDeafened;
    $: localMuted = !!$callState.localMuted[peer];
    $: name = $peerName(peer);
    $: isCoordinator = peer === $callState.coordinator;

    function openMenu(e) {
        const r = e.currentTarget.getBoundingClientRect();
        dispatch('menu', { peer, x: e.clientX || r.right - 8, y: e.clientY || r.top + 8 });
    }
</script>

<!-- svelte-ignore a11y-no-static-element-interactions -->
<div
    class="group relative w-full h-full min-h-0 rounded-xl overflow-hidden bg-[#1e1f22] transition-shadow duration-150"
    class:speaking
    on:contextmenu|preventDefault={openMenu}
>
    {#if camera}
        <VideoView stream={camera} fit="cover" mirror={isSelf} />
    {:else}
        <div class="absolute inset-0 flex items-center justify-center" style="background:radial-gradient(circle at 50% 40%, rgba(255,255,255,.05), transparent 70%)">
            <Avatar id={peer} {name} src={$peerAvatar(peer)} size={compact ? 44 : 84} {speaking} />
        </div>
    {/if}

    <div class="absolute left-2 bottom-2 max-w-[calc(100%-16px)] flex items-center gap-1.5 rounded-md bg-black/60 backdrop-blur px-2 py-1 text-[13px] font-semibold text-white">
        {#if deaf}
            <Icon name="headphonesOff" size={14} className="text-[#f23f43]" />
        {:else if micOff}
            <Icon name="micOff" size={14} className="text-[#f23f43]" />
        {:else if localMuted}
            <Icon name="volumeOff" size={14} className="text-[#f0b232]" />
        {/if}
        <span class="truncate">{name}</span>
        {#if isCoordinator && !compact}
            <span class="text-[10px] font-bold uppercase tracking-wide text-white/50" title="Coordinates this call's relay tree">host</span>
        {/if}
    </div>

    <button
        class="absolute right-2 top-2 w-8 h-8 rounded-full bg-black/55 text-white flex items-center justify-center opacity-0 group-hover:opacity-100 focus:opacity-100 transition-opacity"
        title="More"
        aria-label="More options for {name}"
        on:click|stopPropagation={openMenu}
    >
        <Icon name="more" size={18} />
    </button>
</div>

<style>
    .speaking {
        box-shadow: inset 0 0 0 3px var(--color-success);
    }
</style>
