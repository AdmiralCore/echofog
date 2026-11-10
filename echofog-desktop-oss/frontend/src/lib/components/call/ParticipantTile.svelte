<script>
    import { createEventDispatcher } from 'svelte';
    import { callState } from '../../services/call.service.js';
    import { peerName, peerAvatar, colorFor } from '../../stores/peers.store.js';
    import { backdropOf } from '../../stores/backdrops.js';
    import Avatar from '../ui/Avatar.svelte';
    import Icon from '../ui/Icon.svelte';
    import VideoView from './VideoView.svelte';
    import TileBackdrop from './TileBackdrop.svelte';
    import { autohide } from './autohide.js';
    import './call-ui.css';
    import { t } from '../../i18n/index.js';

    export let peer;
    /** camera stream of this person, if any */
    export let camera = null;
    export let compact = false;
    /** a narrow strip: the face only, the name in the tooltip */
    export let strip = false;

    const dispatch = createEventDispatcher();
    let boxH = 0;
    // the avatar shrinks with the tile so it never runs under the name label
    $: avatarSize = Math.round(Math.max(28, Math.min(strip ? 36 : compact ? 44 : 72, boxH * 0.4 || 84)));

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
    class="vframe speak-ring tile"
    class:speaking
    class:strip
    style="background:var(--tile)"
    title={strip ? name : undefined}
    bind:clientHeight={boxH}
    use:autohide
    on:contextmenu|preventDefault={openMenu}
>
    {#if camera && !strip}
        <VideoView stream={camera} fit="cover" mirror={isSelf} />
    {:else}
        {#if !strip}<TileBackdrop effect={$backdropOf(peer)} color={colorFor(peer)} {speaking} />{/if}
        <div class="absolute inset-0 flex items-center justify-center">
            <Avatar id={peer} {name} src={$peerAvatar(peer)} size={avatarSize} />
        </div>
    {/if}

    {#if !strip}
        <div class="absolute left-2 bottom-2 max-w-[calc(100%-16px)] vtag" style="height:22px;font-size:var(--fs-xs)">
            {#if deaf}
                <Icon name="headphonesOff" size={13} className="text-[color:var(--danger-fg)]" />
            {:else if micOff}
                <Icon name="micOff" size={13} className="text-[color:var(--danger-fg)]" />
            {:else if localMuted}
                <Icon name="volumeOff" size={13} className="text-[color:var(--warn-fg)]" />
            {/if}
            <span class="truncate">{name}</span>
            {#if isCoordinator && !compact}
                <span class="host" title={$t('call.hostTitle')}>{$t('call.host')}</span>
            {/if}
        </div>

        <button
            class="vbtn ov absolute right-2 top-2"
            title={$t('call.more')}
            aria-label={$t('call.moreFor', { name })}
            on:click|stopPropagation={openMenu}
        >
            <Icon name="more" size={16} />
        </button>
    {:else if micOff || deaf}
        <span class="absolute right-0.5 bottom-0.5 rounded-full p-0.5" style="background:var(--surface-3);color:var(--danger-fg)">
            <Icon name={deaf ? 'headphonesOff' : 'micOff'} size={11} />
        </span>
    {/if}
</div>

<style>
    .host { font-size: 10px; font-weight: var(--fw-bold); text-transform: uppercase; letter-spacing: 0.05em; color: rgba(255, 255, 255, 0.6); }
    .strip { border-radius: var(--r-md); }
</style>
