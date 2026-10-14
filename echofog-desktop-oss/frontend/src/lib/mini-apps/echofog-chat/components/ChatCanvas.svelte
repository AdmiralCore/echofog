<script>
    import { tick, onMount, afterUpdate } from "svelte";
    import { fade } from "svelte/transition";
    import {
        conversations,
        activeChannelId,
        sendMessage,
        sendMicroVoucher,
        claimVoucher,
        rightPanelVisible,
        chatError,
        peerBalances,
        loadDirectConversations,
    } from "../stores/chat.store.js";
    import { sendGroupChatMessage } from "../stores/groups.store.js";
    import {
        isCallActive,
        peerInfo,
        callDuration,
        formatDuration,
        stageExpanded,
        startCall,
    } from "../../../stores/call.store.js";
    import { callState } from "../../../services/call.service.js";
    import { colorFor, peerName } from "../../../stores/peers.store.js";
    import CallStage from "../../../components/call/CallStage.svelte";
    import Avatar from "../../../components/ui/Avatar.svelte";
    import Icon from "../../../components/ui/Icon.svelte";

    const GROUP_GAP_MS = 5 * 60 * 1000;

    let inputMessage = "";
    let textarea;
    let showVoucherModal = false;
    let voucherAmount = 25;
    let voucherWallet = "";
    let messageContainer;
    let atBottom = true;
    let lastCount = 0;
    let lastChat = "";
    let maximized = false;
    let stageHeight = Math.max(320, Math.round(window.innerHeight * 0.58));
    let resizing = null;

    $: activeChat = $conversations.find((c) => c.id === $activeChannelId) || $conversations[0];
    $: messages = activeChat?.messages || [];
    $: days = buildDays(messages, activeChat?.type === "channel");
    $: activeBalance = activeChat?.peerId ? $peerBalances[activeChat.peerId] : undefined;
    $: callHere = $isCallActive && !!activeChat && $peerInfo.chatId === activeChat.id;
    $: busyElsewhere = $isCallActive && !callHere;
    $: canCall = !!activeChat && (activeChat.type === "channel" || !!activeChat.pubKey);
    $: if (!callHere) maximized = false;
    $: subtitle = activeChat
        ? activeChat.type === "channel"
            ? `${activeChat.membersCount || 0} ${activeChat.membersCount === 1 ? "member" : "members"}`
            : activeChat.online ? "online" : "last seen recently"
        : "";

    onMount(loadDirectConversations);

    // Stick to the bottom on new messages unless the user scrolled up to read history.
    afterUpdate(() => {
        if (!messageContainer) return;
        const chatChanged = activeChat?.id !== lastChat;
        if (chatChanged || (messages.length !== lastCount && atBottom)) {
            messageContainer.scrollTop = messageContainer.scrollHeight;
        }
        lastChat = activeChat?.id;
        lastCount = messages.length;
    });

    function onScroll() {
        const el = messageContainer;
        atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    }

    function scrollToBottom() {
        messageContainer?.scrollTo({ top: messageContainer.scrollHeight, behavior: "smooth" });
    }

    function dayLabel(ts) {
        const d = new Date(ts);
        const today = new Date();
        const yesterday = new Date(Date.now() - 86400000);
        if (d.toDateString() === today.toDateString()) return "Today";
        if (d.toDateString() === yesterday.toDateString()) return "Yesterday";
        return d.toLocaleDateString([], { day: "numeric", month: "long", year: d.getFullYear() === today.getFullYear() ? undefined : "numeric" });
    }

    // days -> messages flagged with their position inside a sender group. Each day is its own
    // section so only the current date label sticks, like Telegram.
    function buildDays(list, isGroup) {
        const out = [];
        let cur = null;
        for (let i = 0; i < list.length; i++) {
            const m = list[i];
            const day = new Date(m.ts || Date.now()).toDateString();
            if (!cur || cur.day !== day) {
                cur = { day, key: "d" + day, label: dayLabel(m.ts || Date.now()), rows: [] };
                out.push(cur);
            }
            const prev = list[i - 1];
            const next = list[i + 1];
            const same = (a, b) => a && b && a.isMine === b.isMine && a.sender === b.sender && Math.abs((b.ts || 0) - (a.ts || 0)) < GROUP_GAP_MS &&
                new Date(a.ts || 0).toDateString() === new Date(b.ts || 0).toDateString();
            cur.rows.push({
                key: m.id,
                m,
                first: !same(prev, m),
                last: !same(m, next),
                showSender: isGroup && !m.isMine,
            });
        }
        return out;
    }

    async function handleSend() {
        const text = inputMessage.trim();
        if (!text || !activeChat) return;
        inputMessage = "";
        await tick();
        autosize();
        atBottom = true;
        if (activeChat.type === "channel") await sendGroupChatMessage(activeChat.id, text);
        else await sendMessage(activeChat.id, text);
    }

    function onKeydown(e) {
        if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
            e.preventDefault();
            handleSend();
        }
    }

    function autosize() {
        if (!textarea) return;
        textarea.style.height = "auto";
        textarea.style.height = Math.min(textarea.scrollHeight, 160) + "px";
    }

    async function handleSendVoucher() {
        if (!voucherWallet.trim()) return;
        const ok = await sendMicroVoucher(activeChat.id, voucherAmount, voucherWallet);
        if (ok) showVoucherModal = false;
    }

    function callTarget() {
        if (activeChat?.type === "channel") {
            return { alias: activeChat.name, peerId: activeChat.id, groupId: activeChat.id, chatId: activeChat.id };
        }
        return { alias: activeChat?.name, peerId: activeChat?.peerId, pubKey: activeChat?.pubKey, chatId: activeChat?.id };
    }

    function startResize(e) {
        resizing = { y: e.clientY, h: stageHeight };
    }
    function onMove(e) {
        if (!resizing) return;
        stageHeight = Math.max(240, Math.min(window.innerHeight - 220, resizing.h + e.clientY - resizing.y));
    }
</script>

<svelte:window on:mousemove={onMove} on:mouseup={() => (resizing = null)} />

{#if activeChat}
<div class="flex-1 relative flex flex-col h-full overflow-hidden min-w-0 no-drag bg-background">
    <header class="h-14 flex items-center px-4 gap-3 justify-between border-b border-border shrink-0 z-10 bg-background">
        <button class="flex items-center gap-3 min-w-0 text-left" on:click={() => ($rightPanelVisible = !$rightPanelVisible)}>
            <Avatar id={activeChat.peerId || activeChat.id} name={activeChat.name} size={38} />
            <div class="min-w-0">
                <h3 class="text-[15px] font-bold truncate leading-tight">{activeChat.name}</h3>
                <p class="text-[13px] truncate leading-tight {activeChat.online ? 'text-accent' : 'text-textMuted'}">
                    {#if callHere}<span class="text-success">In a call · {formatDuration($callDuration)}</span>{:else}{subtitle}{/if}
                    {#if activeBalance !== undefined}<span class="text-textMuted"> · relay balance {activeBalance}</span>{/if}
                </p>
            </div>
        </button>
        <div class="flex items-center gap-0.5">
            {#if callHere && !$stageExpanded}
                <button class="h-8 px-3 mr-1 rounded-full bg-[rgba(0,186,124,0.15)] text-success text-sm font-semibold flex items-center gap-1.5 hover:bg-[rgba(0,186,124,0.25)]" on:click={() => stageExpanded.set(true)}>
                    <Icon name="phone" size={15} /> Show call
                </button>
            {/if}
            <button class="ef-icon-btn" disabled={!canCall || $isCallActive} title={busyElsewhere ? "You are already in a call" : "Start voice call"} aria-label="Voice call" on:click={() => startCall(callTarget(), false)}>
                <Icon name="phone" size={20} />
            </button>
            <button class="ef-icon-btn" disabled={!canCall || $isCallActive} title={busyElsewhere ? "You are already in a call" : "Start video call"} aria-label="Video call" on:click={() => startCall(callTarget(), true)}>
                <Icon name="video" size={21} />
            </button>
            <button class="ef-icon-btn {$rightPanelVisible ? '!text-accent' : ''}" title="Chat info" aria-label="Chat info" on:click={() => ($rightPanelVisible = !$rightPanelVisible)}>
                <Icon name="panel" size={20} />
            </button>
        </div>
    </header>

    {#if callHere && $stageExpanded}
        <div class="shrink-0 {maximized ? 'flex-1 min-h-0' : ''}" style={maximized ? '' : `height:${stageHeight}px`}>
            <CallStage title={activeChat.name} bind:maximized />
        </div>
        {#if !maximized}
            <!-- svelte-ignore a11y-no-static-element-interactions -->
            <div class="h-1.5 shrink-0 cursor-row-resize bg-black hover:bg-[var(--color-accent)] transition-colors" title="Drag to resize" on:mousedown={startResize}></div>
        {/if}
    {/if}

    {#if !maximized}
        {#if $chatError}
            <div class="px-4 py-2 text-sm text-danger border-b border-border shrink-0">{$chatError}</div>
        {/if}

        <div bind:this={messageContainer} on:scroll={onScroll} class="flex-1 overflow-y-auto px-4 py-3 chat-bg">
            {#if days.length === 0}
                <div class="h-full flex flex-col items-center justify-center text-center gap-2 text-textMuted">
                    <Avatar id={activeChat.peerId || activeChat.id} name={activeChat.name} size={72} />
                    <p class="text-text font-semibold mt-2">{activeChat.name}</p>
                    <p class="text-sm max-w-xs">No messages yet. Messages and calls here are end-to-end encrypted.</p>
                </div>
            {/if}
            <div class="max-w-3xl mx-auto flex flex-col">
                {#each days as day (day.key)}
                <section class="flex flex-col">
                        <div class="sticky top-0 z-[1] flex justify-center py-2 pointer-events-none">
                            <span class="px-3 py-1 rounded-full bg-surface text-xs font-semibold text-textMuted border border-border">{day.label}</span>
                        </div>
                    {#each day.rows as row (row.key)}
                    {#if row.m.type === "voucher"}
                        <div class="flex {row.m.isMine ? 'justify-end' : 'justify-start'} my-1.5">
                            <div class="w-64 rounded-2xl p-4 border border-border bg-surface">
                                <p class="text-xs text-textMuted mb-1">{row.m.isMine ? "You sent" : "You received"}</p>
                                <p class="text-2xl font-extrabold">{row.m.voucher?.amount} <span class="text-sm font-semibold text-textMuted">FOG</span></p>
                                {#if row.m.voucher?.memo}<p class="text-sm text-textMuted mt-1">{row.m.voucher.memo}</p>{/if}
                                {#if !row.m.isMine}
                                    <button class="ef-btn w-full mt-3 !h-9 text-sm" disabled={row.m.voucher?.claimed} on:click={() => !row.m.voucher?.claimed && claimVoucher(activeChat.id, row.m.voucher?.id)}>
                                        {row.m.voucher?.claimed ? "Received" : "Accept"}
                                    </button>
                                {/if}
                                <p class="text-[11px] text-textMuted mt-2 text-right">{row.m.time}</p>
                            </div>
                        </div>
                    {:else}
                        <div class="flex items-end gap-2 {row.m.isMine ? 'justify-end' : 'justify-start'} {row.first ? 'mt-2' : 'mt-0.5'}">
                            {#if row.showSender}
                                <div class="w-8 shrink-0">
                                    {#if row.last}<Avatar id={row.m.senderId || row.m.sender} name={row.m.senderId ? $peerName(row.m.senderId) : row.m.sender} size={32} />{/if}
                                </div>
                            {/if}
                            <div
                                class="bubble {row.m.isMine ? 'mine' : 'theirs'} {row.last ? 'tail' : ''}"
                                class:failed={row.m.status === "failed"}
                            >
                                {#if row.showSender && row.first}
                                    <p class="text-[13px] font-semibold mb-0.5" style="color:{colorFor(row.m.senderId || row.m.sender)}">{row.m.senderId ? $peerName(row.m.senderId) : row.m.sender}</p>
                                {/if}
                                <span class="whitespace-pre-wrap break-words select-text">{row.m.text}</span>
                                <span class="meta">
                                    {row.m.time}
                                    {#if row.m.isMine}
                                        {#if row.m.status === "failed"}<span class="text-danger font-bold" title="Not delivered">!</span>{:else}<Icon name="checks" size={14} stroke={2} />{/if}
                                    {/if}
                                </span>
                            </div>
                        </div>
                    {/if}
                    {/each}
                </section>
                {/each}
            </div>
        </div>

        {#if !atBottom}
            <button class="absolute right-6 bottom-24 w-10 h-10 rounded-full bg-surface border border-border shadow-lg flex items-center justify-center text-textMuted hover:text-text" aria-label="Jump to latest" transition:fade={{ duration: 120 }} on:click={scrollToBottom}>
                <Icon name="arrowDown" size={18} />
            </button>
        {/if}

        {#if showVoucherModal}
            <div class="absolute bottom-20 left-4 w-72 ef-card bg-background p-4 shadow-xl z-30 flex flex-col gap-3">
                <div class="flex items-center justify-between">
                    <span class="text-[15px] font-bold">Send tokens</span>
                    <button class="ef-icon-btn !w-7 !h-7" aria-label="Close" on:click={() => (showVoucherModal = false)}><Icon name="close" size={16} /></button>
                </div>
                <div class="grid grid-cols-4 gap-1.5">
                    {#each [5, 25, 50, 100] as amt}
                        <button class="py-1.5 text-sm rounded-full font-semibold border transition-colors {voucherAmount === amt ? 'bg-text text-background border-transparent' : 'border-border ef-row'}" on:click={() => (voucherAmount = amt)}>{amt}</button>
                    {/each}
                </div>
                <input type="text" bind:value={voucherWallet} placeholder="Recipient wallet address" class="ef-input !py-2 !text-sm font-mono" />
                <button class="ef-btn ef-btn-accent w-full" disabled={!voucherWallet.trim()} on:click={handleSendVoucher}>Send {voucherAmount} FOG</button>
            </div>
        {/if}

        <div class="px-4 pb-4 pt-2 shrink-0">
            <div class="max-w-3xl mx-auto flex items-end gap-2 rounded-2xl bg-surface border border-border px-2 py-1.5 focus-within:border-accent transition-colors">
                <button class="ef-icon-btn shrink-0 mb-0.5" title="Send tokens" aria-label="Send tokens" on:click={() => (showVoucherModal = !showVoucherModal)}>
                    <Icon name="coin" size={21} />
                </button>
                <textarea
                    bind:this={textarea}
                    bind:value={inputMessage}
                    on:input={autosize}
                    on:keydown={onKeydown}
                    rows="1"
                    placeholder={activeChat.readOnly && activeChat.myRole !== "owner" ? "Only admins can post here" : "Write a message…"}
                    disabled={activeChat.readOnly && activeChat.myRole !== "owner" && activeChat.myRole !== "admin"}
                    class="flex-1 resize-none bg-transparent outline-none py-2 text-[15px] leading-5 placeholder:text-textMuted select-text max-h-40"
                ></textarea>
                <button
                    class="w-9 h-9 mb-0.5 rounded-full flex items-center justify-center shrink-0 transition-colors {inputMessage.trim() ? 'bg-accent text-white' : 'text-textMuted'}"
                    disabled={!inputMessage.trim()}
                    on:click={handleSend}
                    title="Send (Enter)"
                    aria-label="Send"
                >
                    <Icon name="send" size={19} />
                </button>
            </div>
        </div>
    {/if}
</div>
{:else}
<div class="flex-1 h-full flex flex-col items-center justify-center gap-2 bg-background text-textMuted text-sm">
    <Icon name="users" size={40} stroke={1.4} />
    <p>Add a contact or join a group to start chatting</p>
</div>
{/if}

<style>
    .chat-bg {
        background-image: radial-gradient(var(--color-border) 0.5px, transparent 0.5px);
        background-size: 18px 18px;
    }
    .bubble {
        position: relative;
        max-width: min(560px, 78%);
        padding: 6px 10px 6px 12px;
        border-radius: 18px;
        font-size: 15px;
        line-height: 1.35;
    }
    .bubble.theirs {
        background: var(--color-bubble-in);
        color: var(--color-text);
        border: 1px solid var(--color-border);
    }
    .bubble.mine {
        background: var(--color-bubble-out);
        color: var(--color-bubble-out-text);
    }
    .bubble.theirs.tail {
        border-bottom-left-radius: 6px;
    }
    .bubble.mine.tail {
        border-bottom-right-radius: 6px;
    }
    .bubble.failed {
        opacity: 0.6;
    }
    .meta {
        float: right;
        display: inline-flex;
        align-items: center;
        gap: 3px;
        margin: 6px 0 -4px 10px;
        font-size: 11px;
        opacity: 0.7;
        white-space: nowrap;
    }
</style>
