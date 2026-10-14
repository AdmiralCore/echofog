<script>
    import { onMount } from "svelte";
    import { conversations, activeChannelId, activeFilter } from "../stores/chat.store.js";
    import { loadGroupConversations, createGroup, joinGroup } from "../stores/groups.store.js";
    import {
        isCallActive,
        peerInfo,
        callDuration,
        formatDuration,
        stageExpanded,
        toggleMute,
        toggleDeafen,
        endCall,
    } from "../../../stores/call.store.js";
    import { callState } from "../../../services/call.service.js";
    import { selfId, loadPeerDirectory, shortId, peerName } from "../../../stores/peers.store.js";
    import Avatar from "../../../components/ui/Avatar.svelte";
    import Icon from "../../../components/ui/Icon.svelte";

    let searchQuery = "";
    let formMode = "";
    let plusOpen = false;
    let newTitle = "";
    let newDescription = "";
    let newKind = "group";
    let inviteText = "";
    let busy = false;
    let copied = false;

    onMount(() => {
        loadGroupConversations();
        loadPeerDirectory();
    });

    function openForm(mode) {
        formMode = mode;
        plusOpen = false;
    }

    async function submitCreate() {
        if (!newTitle.trim() || busy) return;
        busy = true;
        const g = await createGroup(newTitle, newDescription, newKind);
        busy = false;
        if (g) {
            newTitle = "";
            newDescription = "";
            formMode = "";
        }
    }

    async function submitJoin() {
        if (!inviteText.trim() || busy) return;
        busy = true;
        const g = await joinGroup(inviteText);
        busy = false;
        if (g) {
            inviteText = "";
            formMode = "";
        }
    }

    async function copyMyId() {
        try {
            await navigator.clipboard.writeText($selfId);
            copied = true;
            setTimeout(() => (copied = false), 1500);
        } catch (_) { /* clipboard blocked */ }
    }

    function openCall() {
        if ($peerInfo.chatId) $activeChannelId = $peerInfo.chatId;
        stageExpanded.set(true);
    }

    function preview(chat) {
        const last = chat.messages?.[chat.messages.length - 1];
        if (!last) return chat.lastMessage || "";
        const text = last.type === "voucher" ? "Sent tokens" : last.text;
        if (last.isMine) return `You: ${text}`;
        if (chat.type !== "channel") return text;
        const who = last.senderId ? $peerName(last.senderId) : last.sender;
        return `${who.split(" ")[0]}: ${text}`;
    }

    $: filteredChats = $conversations.filter((c) => {
        const matchesFilter =
            $activeFilter === "all" ||
            ($activeFilter === "direct" && c.type === "direct") ||
            ($activeFilter === "channel" && c.type === "channel");
        const q = searchQuery.trim().toLowerCase();
        return matchesFilter && (!q || c.name.toLowerCase().includes(q) || (c.lastMessage || "").toLowerCase().includes(q));
    });

    const filterTabs = [
        { id: "all", label: "All" },
        { id: "direct", label: "People" },
        { id: "channel", label: "Groups" },
    ];
</script>

<div class="flex flex-col h-full w-full select-none no-drag bg-background">
    <div class="px-3 pt-3 pb-2 shrink-0">
        <div class="flex items-center gap-2">
            <div class="relative flex-1">
                <span class="absolute left-3 top-1/2 -translate-y-1/2 text-textMuted"><Icon name="search" size={16} /></span>
                <input type="text" bind:value={searchQuery} placeholder="Search"
                    class="w-full h-9 rounded-full bg-surface border border-transparent focus:border-accent outline-none pl-9 pr-3 text-[14px] placeholder:text-textMuted transition-colors select-text" />
            </div>
            <div class="relative">
                <button class="ef-icon-btn !w-9 !h-9 {plusOpen ? '!bg-[var(--color-hover)] !text-text' : ''}" title="New chat" aria-label="New chat" aria-expanded={plusOpen} on:click={() => (plusOpen = !plusOpen)}>
                    <Icon name="plus" size={20} />
                </button>
                {#if plusOpen}
                    <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
                    <div class="fixed inset-0 z-30" on:click={() => (plusOpen = false)}></div>
                    <div class="absolute right-0 top-11 z-40 w-52 ef-card bg-elevated shadow-xl p-1.5" role="menu">
                        <button class="w-full flex items-center gap-2.5 px-3 py-2 rounded-lg ef-row text-sm text-left" role="menuitem" on:click={() => openForm("create")}>
                            <Icon name="users" size={18} /> New group
                        </button>
                        <button class="w-full flex items-center gap-2.5 px-3 py-2 rounded-lg ef-row text-sm text-left" role="menuitem" on:click={() => openForm("join")}>
                            <Icon name="plus" size={18} /> Join with invite
                        </button>
                    </div>
                {/if}
            </div>
        </div>

        {#if formMode === "create"}
            <div class="ef-card p-3 mt-3 space-y-2">
                <div class="flex items-center justify-between">
                    <span class="font-bold text-sm">New group</span>
                    <button class="ef-icon-btn !w-7 !h-7" aria-label="Cancel" on:click={() => (formMode = "")}><Icon name="close" size={15} /></button>
                </div>
                <input class="ef-input !py-2" placeholder="Group name" maxlength="64" bind:value={newTitle} />
                <input class="ef-input !py-2" placeholder="Description (optional)" bind:value={newDescription} />
                <select class="ef-input !py-2" bind:value={newKind}>
                    <option value="group">Private group</option>
                    <option value="channel">Channel (only admins post)</option>
                    <option value="swarm">Open community</option>
                </select>
                <button class="ef-btn ef-btn-accent w-full !h-9" disabled={busy || !newTitle.trim()} on:click={submitCreate}>Create</button>
            </div>
        {:else if formMode === "join"}
            <div class="ef-card p-3 mt-3 space-y-2">
                <div class="flex items-center justify-between">
                    <span class="font-bold text-sm">Join with invite</span>
                    <button class="ef-icon-btn !w-7 !h-7" aria-label="Cancel" on:click={() => (formMode = "")}><Icon name="close" size={15} /></button>
                </div>
                <input class="ef-input !py-2 font-mono" placeholder="Paste an invite" bind:value={inviteText} />
                <button class="ef-btn ef-btn-accent w-full !h-9" disabled={busy || !inviteText.trim()} on:click={submitJoin}>Join</button>
            </div>
        {/if}

        <div class="flex items-center gap-1 mt-3">
            {#each filterTabs as tab}
                <button
                    class="px-3 h-7 rounded-full text-[13px] font-semibold shrink-0 transition-colors {$activeFilter === tab.id ? 'bg-accent text-white' : 'text-textMuted ef-row'}"
                    on:click={() => ($activeFilter = tab.id)}>{tab.label}</button>
            {/each}
        </div>
    </div>

    <div class="flex-1 overflow-y-auto px-1.5 pb-2">
        {#if filteredChats.length === 0}
            <div class="py-16 text-center text-textMuted text-sm">{searchQuery ? "Nothing found" : "No chats yet"}</div>
        {:else}
            {#each filteredChats as chat (chat.id)}
                {@const inCall = $isCallActive && $peerInfo.chatId === chat.id}
                <button
                    class="w-full flex items-center gap-3 px-2.5 py-2 rounded-xl text-left transition-colors {$activeChannelId === chat.id ? 'bg-accent text-white' : 'ef-row'}"
                    on:click={() => ($activeChannelId = chat.id)}>
                    <div class="relative shrink-0">
                        <Avatar id={chat.peerId || chat.id} name={chat.name} size={48} />
                        {#if chat.online && chat.type === "direct"}
                            <span class="absolute bottom-0 right-0 w-3.5 h-3.5 rounded-full bg-success border-[3px] {$activeChannelId === chat.id ? 'border-accent' : 'border-background'}"></span>
                        {/if}
                    </div>
                    <div class="flex-1 min-w-0">
                        <div class="flex justify-between items-baseline gap-2">
                            <span class="font-semibold text-[15px] truncate flex items-center gap-1">
                                {#if chat.type === "channel"}<Icon name="users" size={14} className="opacity-70" />{/if}
                                {chat.name}
                            </span>
                            <span class="text-xs shrink-0 {$activeChannelId === chat.id ? 'text-white/80' : 'text-textMuted'}">{chat.time}</span>
                        </div>
                        <div class="flex justify-between items-center gap-2">
                            {#if inCall}
                                <span class="text-sm truncate flex items-center gap-1 {$activeChannelId === chat.id ? 'text-white' : 'text-success'}"><Icon name="phone" size={13} /> Call in progress</span>
                            {:else}
                                <span class="text-sm truncate {$activeChannelId === chat.id ? 'text-white/80' : 'text-textMuted'}">{preview(chat) || (chat.type === "channel" ? chat.statusText : "No messages yet")}</span>
                            {/if}
                            {#if chat.unread > 0}
                                <span class="shrink-0 bg-accent text-white text-xs font-bold px-1.5 rounded-full min-w-[20px] h-5 flex items-center justify-center">{chat.unread}</span>
                            {/if}
                        </div>
                    </div>
                </button>
            {/each}
        {/if}
    </div>

    {#if $isCallActive}
        <div class="mx-2 mb-2 rounded-xl bg-surface border border-border p-2.5">
            <div class="flex items-center gap-2">
                <Icon name="signal" size={18} className={$callState.inCall ? "text-success" : "text-textMuted animate-pulse"} />
                <button class="flex-1 min-w-0 text-left" on:click={openCall}>
                    <p class="text-[13px] font-bold leading-tight {$callState.inCall ? 'text-success' : 'text-textMuted'}">{$callState.inCall ? "Voice connected" : "Connecting…"}</p>
                    <p class="text-xs text-textMuted truncate hover:underline">{$peerInfo.alias || "Call"}{#if $callState.inCall} · {formatDuration($callDuration)}{/if}</p>
                </button>
                <button class="ef-icon-btn !w-8 !h-8 hover:!text-danger" title="Leave call" aria-label="Leave call" on:click={endCall}>
                    <Icon name="hangup" size={18} />
                </button>
            </div>
        </div>
    {/if}

    <div class="h-14 shrink-0 border-t border-border px-2.5 flex items-center gap-2.5">
        <button class="flex items-center gap-2.5 min-w-0 flex-1 rounded-lg px-1.5 py-1 ef-row text-left" title="Copy your ID" on:click={copyMyId}>
            <Avatar id={$selfId} name="You" size={34} speaking={!!$callState.speaking[$selfId]} />
            <div class="min-w-0">
                <p class="text-[14px] font-semibold leading-tight">You</p>
                <p class="text-xs text-textMuted truncate font-mono">{copied ? "Copied!" : shortId($selfId) || "offline"}</p>
            </div>
        </button>
        <button class="ef-icon-btn !w-8 !h-8 {$callState.isMicMuted ? '!text-danger' : ''}" disabled={!$isCallActive} title={$callState.isMicMuted ? "Unmute" : "Mute"} aria-label="Toggle microphone" on:click={toggleMute}>
            <Icon name={$callState.isMicMuted ? "micOff" : "mic"} size={18} />
        </button>
        <button class="ef-icon-btn !w-8 !h-8 {$callState.isDeafened ? '!text-danger' : ''}" disabled={!$isCallActive} title={$callState.isDeafened ? "Undeafen" : "Deafen"} aria-label="Toggle sound" on:click={toggleDeafen}>
            <Icon name={$callState.isDeafened ? "headphonesOff" : "headphones"} size={18} />
        </button>
    </div>
</div>
