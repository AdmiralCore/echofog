<script>
    import { onMount, onDestroy } from "svelte";
    import { fade, fly } from "svelte/transition";
    import QRCode from "qrcode";
    import { GetTonWallet, GetTonBalance, GetTonHistory, SendTon, SendUsdt, GetTonTopUpLink, ExportTonRecoveryPhrase, GetSupport, SetSupportPercent, GetOnRampURL } from "@wailsjs/go/main/App";
    import { BrowserOpenURL } from "@wailsjs/runtime/runtime";
    import Icon from "../ui/Icon.svelte";

    const REFRESH_MS = 15000;
    const QUICK_AMOUNTS = ["0.5", "1", "5"];

    let wallet = null;
    let balance = null;
    let history = [];
    let loading = true;
    let netError = "";
    let timer;

    let sheet = ""; // "" | "receive" | "send" | "export"
    let menuOpen = false;
    let exportPassword = "";
    let exportError = "";
    let phrase = [];
    let sendAsset = "TON";
    let qr = "";
    let receiveAmount = "";
    let copied = false;

    let sendTo = "";
    let sendAmount = "";

    // voluntary project share of traffic payments; hidden when the build has no project address
    const SHARE_CHOICES = [0, 5, 10];
    let support = { available: false, percent: 0, pendingTon: "0", pendingUsdt: "0" };
    let supportError = "";
    let onRampURL = "";

    async function loadSupport() {
        try { support = await GetSupport(); } catch (e) {}
        try { onRampURL = await GetOnRampURL(); } catch (e) {}
    }

    async function chooseShare(p) {
        supportError = "";
        try {
            await SetSupportPercent(p);
            support = await GetSupport();
        } catch (e) {
            supportError = String(e?.message || e);
        }
    }

    $: pendingShare = Number(support.pendingUsdt) > 0 || Number(support.pendingTon) > 0;
    let sendComment = "";
    let sendStep = "form"; // form | review | sending | done
    let sendError = "";
    let sentHash = "";

    $: testnet = wallet?.network === "testnet";
    $: hasUsdt = !!balance?.usdtAvailable;
    $: tonUsd = balance && balance.usdRate ? Number(balance.ton) * balance.usdRate : null;
    $: usdtValue = hasUsdt ? Number(balance.usdt) : 0;
    $: totalUsd = tonUsd !== null ? tonUsd + usdtValue : null;
    $: available = sendAsset === "USDT" ? Number(balance?.usdt || 0) : Number(balance?.ton || 0);
    $: explorer = testnet ? "https://testnet.tonviewer.com" : "https://tonviewer.com";

    function errText(err) {
        return (err && (err.message || String(err))) || "Something went wrong";
    }

    function fmt(ton) {
        const n = Number(ton);
        if (!isFinite(n)) return ton;
        return n.toLocaleString(undefined, { maximumFractionDigits: n < 1 ? 4 : 2 });
    }

    function short(addr) {
        return addr ? `${addr.slice(0, 6)}…${addr.slice(-6)}` : "";
    }

    function when(sec) {
        const d = new Date(sec * 1000);
        const today = new Date().toDateString() === d.toDateString();
        return today ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : d.toLocaleDateString([], { day: "numeric", month: "short" });
    }

    async function refresh(full = true) {
        try {
            if (!wallet) wallet = await GetTonWallet();
        } catch (err) {
            netError = errText(err);
            loading = false;
            return;
        }
        try {
            balance = await GetTonBalance();
            if (full) history = (await GetTonHistory(20)) || [];
            netError = "";
        } catch (err) {
            netError = errText(err);
        } finally {
            loading = false;
        }
    }

    onMount(() => {
        loadSupport();
        refresh();
        timer = setInterval(() => refresh(), REFRESH_MS);
    });
    onDestroy(() => clearInterval(timer));

    async function openReceive(amount = "") {
        sheet = "receive";
        receiveAmount = amount;
        await renderQR();
    }

    async function renderQR() {
        try {
            const link = await GetTonTopUpLink(receiveAmount);
            qr = await QRCode.toDataURL(link, { margin: 1, width: 240, color: { dark: "#000000", light: "#ffffff" } });
        } catch (err) {
            qr = "";
        }
    }

    async function copyAddress() {
        try {
            await navigator.clipboard.writeText(wallet.address);
            copied = true;
            setTimeout(() => (copied = false), 1500);
        } catch (_) { /* clipboard blocked */ }
    }

    async function openInWalletApp() {
        const link = await GetTonTopUpLink(receiveAmount);
        BrowserOpenURL(link);
    }

    function openSend(asset = "TON") {
        sendAsset = asset;
        sheet = "send";
        sendStep = "form";
        sendError = "";
        sentHash = "";
    }

    function validateSend() {
        if (!sendTo.trim()) return "Enter the recipient address";
        const n = Number(String(sendAmount).replace(",", "."));
        if (!(n > 0)) return "Enter an amount greater than zero";
        if (balance && n > available) return "That is more than your balance";
        if (sendAsset === "USDT" && balance && Number(balance.ton) < 0.065) return "USDT transfers need about 0.06 TON for the network fee. Top up a little TON first.";
        return "";
    }

    function review() {
        sendError = validateSend();
        if (!sendError) sendStep = "review";
    }

    function setMax() {
        if (!balance) return;
        if (sendAsset === "USDT") {
            sendAmount = balance.usdt;
            return;
        }
        const max = Math.max(0, Number(balance.ton) - 0.02); // keep room for the network fee
        sendAmount = max > 0 ? String(Math.floor(max * 1e4) / 1e4) : "";
    }

    async function confirmSend() {
        sendStep = "sending";
        sendError = "";
        try {
            const send = sendAsset === "USDT" ? SendUsdt : SendTon;
            sentHash = await send(sendTo.trim(), String(sendAmount).replace(",", "."), sendComment.trim());
            sendStep = "done";
            sendTo = sendAmount = sendComment = "";
            setTimeout(() => refresh(), 6000);
        } catch (err) {
            sendError = errText(err);
            sendStep = "review";
        }
    }

    function close() {
        sheet = "";
        phrase = [];
        exportPassword = "";
        exportError = "";
    }

    function openExport() {
        menuOpen = false;
        sheet = "export";
        phrase = [];
    }

    async function revealPhrase() {
        exportError = "";
        try {
            phrase = (await ExportTonRecoveryPhrase(exportPassword)) || [];
            exportPassword = "";
        } catch (err) {
            exportError = errText(err).includes("password") ? "Wrong password" : errText(err);
        }
    }
</script>

<div class="w-full h-full overflow-y-auto bg-background text-text no-drag">
    <div class="max-w-md mx-auto px-5 py-6 flex flex-col gap-5">
        <header class="flex items-center justify-between">
            <div class="flex items-center gap-2">
                <h1 class="text-xl font-extrabold">Wallet</h1>
                {#if testnet}<span class="text-[11px] font-bold uppercase px-2 py-0.5 rounded-full bg-[#f0b232] text-black">Testnet</span>{/if}
            </div>
            <div class="flex items-center gap-1 relative">
                <button class="ef-icon-btn" title="Refresh" aria-label="Refresh" on:click={() => refresh()}>
                    <svg class="w-5 h-5 {loading ? 'animate-spin' : ''}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 11-3-6.7L21 8M21 3v5h-5"/></svg>
                </button>
                <button class="ef-icon-btn" title="More" aria-label="More" aria-expanded={menuOpen} on:click={() => (menuOpen = !menuOpen)}><Icon name="more" size={20} /></button>
                {#if menuOpen}
                    <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
                    <div class="fixed inset-0 z-30" on:click={() => (menuOpen = false)}></div>
                    <div class="absolute right-0 top-10 z-40 w-64 ef-card bg-elevated shadow-xl p-1.5" role="menu">
                        <button class="w-full flex items-center gap-2.5 px-3 py-2.5 rounded-lg ef-row text-sm text-left" role="menuitem" on:click={openExport}>
                            <Icon name="lock" size={18} /> Export recovery phrase
                        </button>
                        {#if wallet}
                            <button class="w-full flex items-center gap-2.5 px-3 py-2.5 rounded-lg ef-row text-sm text-left" role="menuitem" on:click={() => { menuOpen = false; BrowserOpenURL(`${explorer}/${wallet.address}`); }}>
                                <Icon name="search" size={18} /> View in explorer
                            </button>
                        {/if}
                    </div>
                {/if}
            </div>
        </header>

        <div class="rounded-2xl border border-[#f0b232]/50 bg-[#f0b232]/10 p-3.5 text-[13px] leading-snug flex gap-3" role="note">
            <span class="font-extrabold text-[#c98a00]" aria-hidden="true">!</span>
            <p>
                <b>This wallet is for paying for traffic only.</b> Top it up with what you plan to spend on proxy nodes and nothing more. Don't keep savings here or use it for anything else, and keep your recovery phrase offline.
            </p>
        </div>

        <section class="rounded-3xl p-6 text-white flex flex-col items-center text-center gap-1" style="background:linear-gradient(140deg,#0098ea 0%,#0063b4 100%)">
            <span class="text-sm text-white/80">Total balance</span>
            {#if balance && totalUsd !== null}
                <p class="text-[40px] leading-tight font-extrabold tracking-tight">${totalUsd.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}</p>
            {:else if balance}
                <p class="text-[40px] leading-tight font-extrabold tracking-tight">{fmt(balance.ton)} <span class="text-2xl font-bold text-white/80">TON</span></p>
            {:else if loading}
                <div class="h-12 w-40 rounded-xl bg-white/20 animate-pulse my-1"></div>
            {:else}
                <p class="text-[40px] leading-tight font-extrabold">— <span class="text-2xl font-bold text-white/80">TON</span></p>
            {/if}
            {#if wallet}
                <button class="mt-2 flex items-center gap-1.5 text-sm text-white/85 hover:text-white font-mono" title="Copy address" on:click={copyAddress}>
                    {copied ? "Address copied" : short(wallet.address)} <Icon name="copy" size={14} />
                </button>
            {/if}
            <div class="grid grid-cols-2 gap-3 w-full mt-5">
                <button class="h-12 rounded-2xl bg-white/20 hover:bg-white/30 font-bold flex items-center justify-center gap-2" on:click={() => openReceive()}>
                    <Icon name="arrowDown" size={18} stroke={2.4} /> Top up
                </button>
                <button class="h-12 rounded-2xl bg-white text-[#0077cc] hover:bg-white/90 font-bold flex items-center justify-center gap-2" on:click={() => openSend("TON")}>
                    <Icon name="send" size={17} stroke={2.2} /> Send
                </button>
            </div>
        </section>

        {#if support.available}
            <section class="rounded-2xl border border-border p-4">
                <div class="flex items-start justify-between gap-3">
                    <div class="min-w-0">
                        <p class="font-semibold">Support EchoFog</p>
                        <p class="text-xs text-textMuted mt-0.5 leading-snug">
                            {#if support.percent > 0}
                                {support.percent}% is added on top of what you pay proxy nodes. Nodes always get their full price. Optional: you can turn it off any time and everything works the same.
                            {:else}
                                Off. Everything works the same without it. Thank you for using EchoFog.
                            {/if}
                        </p>
                    </div>
                    <div class="flex gap-1 shrink-0" role="group" aria-label="Support share">
                        {#each SHARE_CHOICES as p}
                            <button class="chip {support.percent === p ? 'chip-on' : ''}" aria-pressed={support.percent === p} on:click={() => chooseShare(p)}>{p === 0 ? "Off" : p + "%"}</button>
                        {/each}
                    </div>
                </div>
                {#if pendingShare}
                    <p class="text-xs text-textMuted mt-3">Set aside so far: {Number(support.pendingUsdt) > 0 ? support.pendingUsdt + " USDT" : ""}{Number(support.pendingUsdt) > 0 && Number(support.pendingTon) > 0 ? " + " : ""}{Number(support.pendingTon) > 0 ? support.pendingTon + " TON" : ""}. It is sent in one transfer once it is worth the network fee, so small payments don't pay fees for a few cents.</p>
                {/if}
                {#if supportError}<p class="text-xs text-danger mt-2">{supportError}</p>{/if}
            </section>
        {/if}

        {#if netError}
            <div class="rounded-2xl border border-border bg-surface p-4 text-sm flex gap-3 items-start">
                <span class="text-danger font-bold">!</span>
                <div class="flex-1">
                    <p class="font-semibold">Can't reach the TON network</p>
                    <p class="text-textMuted mt-0.5 break-words">{netError}</p>
                </div>
                <button class="text-accent font-semibold" on:click={() => refresh()}>Retry</button>
            </div>
        {/if}

        {#if balance && !balance.deployed && Number(balance.ton) > 0}
            <p class="text-xs text-textMuted text-center -mt-2">Your wallet contract activates with the first payment you send; that payment includes a small one-time fee.</p>
        {/if}

        {#if balance}
            <section class="rounded-2xl border border-border overflow-hidden divide-y divide-[var(--color-border)]">
                <button class="w-full flex items-center gap-3 px-4 py-3 text-left ef-row" on:click={() => openSend("TON")}>
                    <span class="w-10 h-10 rounded-full bg-[#0098ea] text-white font-extrabold flex items-center justify-center text-xs">TON</span>
                    <div class="flex-1"><p class="font-semibold">Toncoin</p><p class="text-xs text-textMuted">{balance.usdRate ? "$" + balance.usdRate.toFixed(2) : "TON"}</p></div>
                    <div class="text-right"><p class="font-semibold">{fmt(balance.ton)} TON</p>{#if tonUsd !== null}<p class="text-xs text-textMuted">${tonUsd.toFixed(2)}</p>{/if}</div>
                </button>
                {#if hasUsdt}
                    <button class="w-full flex items-center gap-3 px-4 py-3 text-left ef-row" on:click={() => openSend("USDT")}>
                        <span class="w-10 h-10 rounded-full bg-[#26a17b] text-white font-extrabold flex items-center justify-center">₮</span>
                        <div class="flex-1"><p class="font-semibold">Tether USD</p><p class="text-xs text-textMuted">USDT on TON</p></div>
                        <div class="text-right"><p class="font-semibold">{fmt(balance.usdt)} USDT</p><p class="text-xs text-textMuted">${usdtValue.toFixed(2)}</p></div>
                    </button>
                {/if}
            </section>
        {/if}

        <section>
            <div class="flex items-center justify-between mb-2">
                <h2 class="font-bold">History</h2>
                {#if wallet}<button class="text-sm text-accent" on:click={() => BrowserOpenURL(`${explorer}/${wallet.address}`)}>Open in explorer</button>{/if}
            </div>
            {#if history.length === 0}
                <div class="rounded-2xl border border-dashed border-border p-8 text-center text-sm text-textMuted">
                    {#if loading}Loading…{:else}No transactions yet. Top up with a few TON to pay for proxy traffic.{/if}
                </div>
            {:else}
                <div class="rounded-2xl border border-border overflow-hidden divide-y divide-[var(--color-border)]">
                    {#each history as tx (tx.hash)}
                        <button class="w-full flex items-center gap-3 px-4 py-3 text-left ef-row" on:click={() => BrowserOpenURL(`${explorer}/transaction/${tx.hash}`)}>
                            <span class="w-10 h-10 rounded-full flex items-center justify-center shrink-0 {tx.incoming ? 'bg-[rgba(0,186,124,0.15)] text-success' : 'bg-surface text-textMuted'}">
                                <Icon name={tx.incoming ? "arrowDown" : "send"} size={18} />
                            </span>
                            <div class="flex-1 min-w-0">
                                <p class="font-semibold text-[15px]">{tx.incoming ? "Received" : "Sent"}{#if tx.failed} <span class="text-danger text-xs">failed</span>{/if}</p>
                                <p class="text-xs text-textMuted truncate">{tx.comment || (tx.incoming ? "from " : "to ") + short(tx.counterparty)}</p>
                            </div>
                            <div class="text-right shrink-0">
                                <p class="font-semibold {tx.incoming ? 'text-success' : ''}">{tx.incoming ? "+" : "−"}{fmt(tx.ton)} {tx.asset || "TON"}</p>
                                <p class="text-xs text-textMuted">{when(tx.time)}</p>
                            </div>
                        </button>
                    {/each}
                </div>
            {/if}
        </section>
    </div>
</div>

{#if sheet}
    <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
    <div class="fixed inset-0 z-[300] bg-black/60 flex items-end sm:items-center justify-center p-4" transition:fade={{ duration: 120 }} on:click|self={close}>
        <div class="w-full max-w-sm rounded-3xl bg-background border border-border p-5 shadow-2xl" transition:fly={{ y: 24, duration: 160 }} role="dialog" aria-modal="true">
            <div class="flex items-center justify-between mb-4">
                <h3 class="text-lg font-bold">{sheet === "receive" ? "Top up" : sheet === "export" ? "Recovery phrase" : "Send " + sendAsset}</h3>
                <button class="ef-icon-btn" aria-label="Close" on:click={close}><Icon name="close" size={18} /></button>
            </div>

            {#if sheet === "receive"}
                <div class="flex gap-2 mb-4">
                    <button class="chip {receiveAmount === '' ? 'chip-on' : ''}" on:click={() => { receiveAmount = ""; renderQR(); }}>Any</button>
                    {#each QUICK_AMOUNTS as a}
                        <button class="chip {receiveAmount === a ? 'chip-on' : ''}" on:click={() => { receiveAmount = a; renderQR(); }}>{a} TON</button>
                    {/each}
                </div>
                <div class="flex justify-center">
                    {#if qr}<img src={qr} alt="QR code with your TON address" class="w-56 h-56 rounded-2xl bg-white p-2" />{:else}<div class="w-56 h-56 rounded-2xl bg-surface animate-pulse"></div>{/if}
                </div>
                <p class="mt-4 text-xs text-textMuted text-center">Your TON address{testnet ? " (testnet)" : ""}</p>
                <button class="mt-1 w-full font-mono text-[13px] break-all text-center px-3 py-2 rounded-xl bg-surface hover:bg-[var(--color-hover)]" on:click={copyAddress}>
                    {wallet?.address}
                </button>
                <p class="text-center text-xs mt-1 h-4 {copied ? 'text-success' : 'text-transparent'}">Copied</p>
                <div class="grid grid-cols-2 gap-2 mt-2">
                    <button class="ef-btn ef-btn-ghost !h-10 text-sm" on:click={copyAddress}>Copy address</button>
                    <button class="ef-btn ef-btn-accent !h-10 text-sm" on:click={openInWalletApp}>Open wallet app</button>
                </div>
                {#if onRampURL}
                    <button class="ef-btn ef-btn-ghost w-full !h-10 text-sm mt-3" on:click={() => BrowserOpenURL(onRampURL)}>Buy with a bank card</button>
                    <p class="text-[11px] text-textMuted mt-1 text-center">Handled by our payment partner, who may charge a fee. Funds arrive in this wallet.</p>
                {/if}
                <p class="mt-3 text-xs text-textMuted text-center">Top up only what you plan to spend on traffic.</p>
                <div class="mt-4 rounded-2xl bg-surface p-3 text-[13px] text-textMuted leading-snug">
                    <p class="text-text font-semibold mb-1">From Telegram Wallet</p>
                    In @wallet, send TON or USDT to an external address: scan this QR or paste the address. For USDT choose the <b>TON network</b>, not TRON or Ethereum.
                </div>
            {:else if sheet === "export"}
                {#if phrase.length === 0}
                    <p class="text-sm text-textMuted leading-snug">These 24 words restore this TON wallet in Tonkeeper, MyTonWallet or any TON wallet app. Anyone who sees them can take your funds.</p>
                    <label class="block text-xs font-semibold text-textMuted mt-4 mb-1" for="export-pass">App password</label>
                    <input id="export-pass" type="password" class="ef-input !py-2.5" bind:value={exportPassword} on:keydown={(e) => e.key === "Enter" && revealPhrase()} />
                    {#if exportError}<p class="text-danger text-sm mt-2">{exportError}</p>{/if}
                    <button class="ef-btn ef-btn-accent w-full mt-4" disabled={!exportPassword} on:click={revealPhrase}>Show phrase</button>
                {:else}
                    <div class="grid grid-cols-3 gap-1.5 select-text">
                        {#each phrase as word, i}
                            <div class="rounded-lg bg-surface px-2 py-1.5 text-sm"><span class="text-textMuted text-xs mr-1">{i + 1}.</span>{word}</div>
                        {/each}
                    </div>
                    <p class="text-xs text-textMuted mt-3">Write the words down on paper. Your EchoFog recovery phrase restores this wallet too.</p>
                    <button class="ef-btn ef-btn-accent w-full mt-4" on:click={close}>I saved it</button>
                {/if}
            {:else}
                {#if sendStep === "form"}
                    {#if hasUsdt}
                        <div class="grid grid-cols-2 gap-1 p-1 rounded-xl bg-surface mb-3" role="tablist">
                            {#each ["TON", "USDT"] as a}
                                <button role="tab" aria-selected={sendAsset === a} class="h-9 rounded-lg text-sm font-bold {sendAsset === a ? 'bg-background shadow text-text' : 'text-textMuted'}" on:click={() => { sendAsset = a; sendError = ""; }}>{a}</button>
                            {/each}
                        </div>
                    {/if}
                    <label class="block text-xs font-semibold text-textMuted mb-1" for="ton-to">Recipient</label>
                    <input id="ton-to" class="ef-input !py-2.5 font-mono !text-sm" placeholder="UQ… or EQ… address" bind:value={sendTo} />
                    <label class="block text-xs font-semibold text-textMuted mt-3 mb-1" for="ton-amount">Amount</label>
                    <div class="relative">
                        <input id="ton-amount" class="ef-input !py-2.5 pr-24" inputmode="decimal" placeholder="0.00" bind:value={sendAmount} />
                        <span class="absolute right-14 top-1/2 -translate-y-1/2 text-textMuted text-sm">{sendAsset}</span>
                        <button class="absolute right-2 top-1/2 -translate-y-1/2 text-accent text-sm font-bold px-2" on:click={setMax}>Max</button>
                    </div>
                    {#if balance}<p class="text-xs text-textMuted mt-1">Available: {fmt(sendAsset === "USDT" ? balance.usdt : balance.ton)} {sendAsset}</p>{/if}
                    <label class="block text-xs font-semibold text-textMuted mt-3 mb-1" for="ton-comment">Comment (optional, public)</label>
                    <input id="ton-comment" class="ef-input !py-2.5 !text-sm" maxlength="120" placeholder="Visible to everyone on the blockchain" bind:value={sendComment} />
                    {#if sendError}<p class="text-danger text-sm mt-3">{sendError}</p>{/if}
                    <button class="ef-btn ef-btn-accent w-full mt-4" on:click={review}>Continue</button>
                {:else if sendStep === "review" || sendStep === "sending"}
                    <div class="rounded-2xl bg-surface p-4 flex flex-col gap-2 text-sm">
                        <div class="flex justify-between"><span class="text-textMuted">Amount</span><span class="font-bold">{sendAmount} {sendAsset}</span></div>
                        <div class="flex justify-between gap-4"><span class="text-textMuted">To</span><span class="font-mono text-right break-all">{short(sendTo.trim())}</span></div>
                        {#if sendComment}<div class="flex justify-between gap-4"><span class="text-textMuted">Comment</span><span class="text-right break-all">{sendComment}</span></div>{/if}
                        <div class="flex justify-between"><span class="text-textMuted">Network fee</span><span>{sendAsset === "USDT" ? "≈ 0.02–0.05 TON (unused part returns)" : "≈ 0.005–0.015 TON"}</span></div>
                    </div>
                    <p class="text-xs text-textMuted mt-3">Blockchain payments cannot be reversed. Check the address.</p>
                    {#if sendError}<p class="text-danger text-sm mt-3 break-words">{sendError}</p>{/if}
                    <div class="grid grid-cols-2 gap-2 mt-4">
                        <button class="ef-btn ef-btn-ghost" disabled={sendStep === "sending"} on:click={() => (sendStep = "form")}>Back</button>
                        <button class="ef-btn ef-btn-accent" disabled={sendStep === "sending"} on:click={confirmSend}>{sendStep === "sending" ? "Sending…" : "Confirm"}</button>
                    </div>
                {:else}
                    <div class="flex flex-col items-center text-center gap-2 py-4">
                        <span class="w-14 h-14 rounded-full bg-[rgba(0,186,124,0.15)] text-success flex items-center justify-center"><Icon name="check" size={28} stroke={2.4} /></span>
                        <p class="font-bold text-lg">Sent</p>
                        <p class="text-sm text-textMuted">The payment is on its way and will show up in the history in a few seconds.</p>
                        {#if sentHash}<p class="text-[11px] font-mono text-textMuted break-all">{sentHash}</p>{/if}
                        <button class="ef-btn ef-btn-accent w-full mt-3" on:click={close}>Done</button>
                    </div>
                {/if}
            {/if}
        </div>
    </div>
{/if}

<style>
    .chip {
        height: 32px;
        padding: 0 12px;
        border-radius: 9999px;
        font-size: 13px;
        font-weight: 700;
        border: 1px solid var(--color-border);
        color: var(--color-text-muted);
    }
    .chip-on {
        background: var(--color-accent);
        border-color: var(--color-accent);
        color: #fff;
    }
</style>
