<script>
    import { rightPanelVisible, conversations, activeChannelId } from "../stores/chat.store.js";
    import { groupMembers, groupInvite, loadGroupDetails, addGroupMember } from "../stores/groups.store.js";
    import { isCallActive, peerInfo } from "../../../stores/call.store.js";
    import { callState } from "../../../services/call.service.js";
    import { peerName } from "../../../stores/peers.store.js";
    import ParticipantMenu from "../../../components/call/ParticipantMenu.svelte";
    import Avatar from "../../../components/ui/Avatar.svelte";
    import Icon from "../../../components/ui/Icon.svelte";

    let memberKey = "";
    let copied = "";
    let loadedFor = "";
    let menu = null;

    $: currentChat = $conversations.find((c) => c.id === $activeChannelId) || $conversations[0];
    $: callHere = $isCallActive && currentChat && $peerInfo.chatId === currentChat.id;
    $: callMembers = callHere ? ($callState.members.length ? $callState.members : [$callState.self]) : [];

    $: if ($rightPanelVisible && currentChat?.type === "channel" && currentChat.id !== loadedFor) {
        loadedFor = currentChat.id;
        loadGroupDetails(currentChat.id);
    }

    async function copy(text, what) {
        try {
            await navigator.clipboard.writeText(text);
            copied = what;
            setTimeout(() => (copied = ""), 1500);
        } catch (_) { /* clipboard blocked */ }
    }

    async function submitMember() {
        if (!memberKey.trim()) return;
        if (await addGroupMember(currentChat.id, memberKey.trim())) memberKey = "";
    }

    function openMenu(e, peer) {
        const r = e.currentTarget.getBoundingClientRect();
        menu = { peer, x: r.left - 260, y: r.top };
    }
</script>

<aside
    class="bg-background border-l border-border flex flex-col h-full transition-all duration-300 z-20 shrink-0 absolute right-0 xl:relative"
    style="width: {$rightPanelVisible ? '320px' : '0px'}; opacity: {$rightPanelVisible ? '1' : '0'}; pointer-events: {$rightPanelVisible ? 'auto' : 'none'}; overflow: hidden;"
    aria-label="Chat info"
>
    <div class="h-14 px-4 border-b border-border flex items-center justify-between shrink-0">
        <span class="text-[15px] font-bold">{currentChat?.type === "channel" ? "Group info" : "User info"}</span>
        <button class="ef-icon-btn" title="Close" aria-label="Close" on:click={() => ($rightPanelVisible = false)}>
            <Icon name="close" size={18} />
        </button>
    </div>

    {#if currentChat}
    <div class="flex-1 overflow-y-auto text-sm">
        <div class="flex flex-col items-center text-center px-4 pt-6 pb-5 gap-2 border-b border-border">
            <Avatar id={currentChat.peerId || currentChat.id} name={currentChat.name} size={88} />
            <p class="text-lg font-bold mt-1">{currentChat.name}</p>
            <p class="text-textMuted">
                {#if currentChat.type === "channel"}{currentChat.membersCount || 0} members{:else}{currentChat.online ? "online" : "last seen recently"}{/if}
            </p>
            {#if currentChat.description}<p class="text-textMuted">{currentChat.description}</p>{/if}
        </div>

        {#if callHere}
            <section class="px-2 py-3 border-b border-border">
                <p class="px-2 pb-1.5 text-xs font-bold uppercase tracking-wide text-success flex items-center gap-1.5">
                    <Icon name="phone" size={13} /> In the call · {callMembers.length}
                </p>
                {#each callMembers as p (p)}
                    <div class="group flex items-center gap-3 px-2 py-1.5 rounded-lg ef-row">
                        <Avatar id={p} name={$peerName(p)} size={32} speaking={!!$callState.speaking[p]} />
                        <span class="flex-1 truncate font-medium">{$peerName(p)}</span>
                        {#if $callState.localMuted[p]}<Icon name="volumeOff" size={16} className="text-[#f0b232]" />{/if}
                        {#if p === $callState.self && $callState.isMicMuted}<Icon name="micOff" size={16} className="text-danger" />{/if}
                        <button class="ef-icon-btn !w-7 !h-7 opacity-0 group-hover:opacity-100" aria-label="Options for {$peerName(p)}" on:click={(e) => openMenu(e, p)}>
                            <Icon name="more" size={16} />
                        </button>
                    </div>
                {/each}
            </section>
        {/if}

        {#if currentChat.type === "channel"}
            <section class="px-2 py-3 border-b border-border">
                <p class="px-2 pb-1.5 text-xs font-bold uppercase tracking-wide text-textMuted">Members · {$groupMembers.length}</p>
                {#each $groupMembers as m}
                    <div class="flex items-center gap-3 px-2 py-1.5 rounded-lg">
                        <Avatar id={m.peerId} name={m.isMe ? "You" : $peerName(m.peerId)} size={32} />
                        <span class="flex-1 truncate">{m.isMe ? "You" : $peerName(m.peerId)}</span>
                        {#if m.role !== "member"}<span class="text-xs text-accent font-semibold">{m.role}</span>{/if}
                    </div>
                {/each}
                {#if currentChat.myRole === "owner" || currentChat.myRole === "admin"}
                    <div class="px-2 pt-2 space-y-2">
                        <input class="ef-input !py-2 !text-sm font-mono" placeholder="Member public key (hex)" bind:value={memberKey} />
                        <button class="ef-btn ef-btn-ghost w-full !h-9 text-sm" disabled={!memberKey.trim()} on:click={submitMember}>Add member</button>
                    </div>
                {/if}
                {#if $groupInvite}
                    <button class="mx-2 mt-2 w-[calc(100%-16px)] ef-btn ef-btn-accent !h-9 text-sm" on:click={() => copy($groupInvite, "invite")}>
                        {copied === "invite" ? "Invite copied" : "Copy invite link"}
                    </button>
                {/if}
            </section>
        {:else if currentChat.peerId}
            <section class="px-4 py-3 border-b border-border">
                <p class="text-xs font-bold uppercase tracking-wide text-textMuted pb-1">User ID</p>
                <button class="w-full text-left font-mono text-xs break-all text-text hover:text-accent" on:click={() => copy(currentChat.peerId, "id")}>
                    {currentChat.peerId}
                </button>
                {#if copied === "id"}<p class="text-xs text-success mt-1">Copied</p>{/if}
            </section>
        {/if}

        <section class="px-4 py-4 flex gap-3">
            <Icon name="lock" size={20} className="text-success mt-0.5" />
            <div>
                <p class="font-semibold">End-to-end encrypted</p>
                <p class="text-textMuted mt-0.5 leading-snug">Messages and calls are encrypted on your device. Calls are relayed by participants, who cannot read them.</p>
            </div>
        </section>
    </div>
    {/if}
</aside>

{#if menu}
    <ParticipantMenu peer={menu.peer} x={menu.x} y={menu.y} on:close={() => (menu = null)} />
{/if}
