<script>
    import { onMount, onDestroy } from "svelte";
    import { EventsOn } from "@wailsjs/runtime/runtime";
    import {
        GetVPNStatus, StartVPN, StopVPN, GetSharingStatus, StartSharing, StopSharing,
        ListExitNodes, RefreshExitNodes, ProbeExitNode, MeasureUploadCapacity,
        GetProbeDomains, SetProbeDomains, SetWholeComputer, GetExitPayment, SetAutoPay, PayExit,
    } from "@wailsjs/go/main/App";

    let vpn = { running: false, listenAddr: "", relayPeerId: "", activeConns: 0, bytesRx: 0, bytesTx: 0, tunnelReady: false, tunnelNotice: "", wholeComputer: false, wholeComputerSupported: false, wholeComputerError: "" };
    let wholeBusy = false;

    // Prices are stored as micro-USDT per MB (49 is about 5 cents a GB) and shown per GB.
    const usdPerGb = (pricePerMb) => (pricePerMb * 1024) / 1e6;
    const fmtPrice = (pricePerMb) => (pricePerMb > 0 ? "$" + usdPerGb(pricePerMb).toFixed(usdPerGb(pricePerMb) < 0.1 ? 3 : 2) + "/GB" : "free");
    const microPerMb = (usd) => Math.max(0, Math.round(((Number(usd) || 0) * 1e6) / 1024));

    let pay = { active: false, canPay: false, pricePerGb: "0", remainingGb: 0, needsPayment: false, autoPay: false, chunkUsdt: "1", chunkGb: 0, dailyCapUsdt: "5", spentTodayUsdt: "0", pending: false, note: "", lastError: "" };
    let payBusy = false;
    let payForm = { chunk: "1", cap: "5" };
    let payFormTouched = false;
    let payNotice = "";
    let selectedRelay = "auto";
    let busy = false;
    let error = "";
    let copiedProxy = false;
    let showDetails = false;
    let offs = [];

    function fmtBytes(n) {
        if (!n) return "0 B";
        const u = ["B", "KB", "MB", "GB", "TB"];
        let i = 0;
        while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
        return n.toFixed(i === 0 ? 0 : 1) + " " + u[i];
    }

    let exits = [];
    let sharing = { enabled: false, config: { pricePerMb: 0, freeTestKb: 512, uploadKbps: 0 }, lastPublish: 0, expiresAt: 0, lastError: "", accepted: 0, refused: 0, bytes: 0 };
    let form = { price: 0, freeKb: 512, uploadMbps: 0 }; // price: USDT per GB
    let shareBusy = false;
    let exitsBusy = false;
    let probing = "";
    let notice = "";
    let domainsText = "";

    async function refreshExits(rescan = false) {
        exitsBusy = true;
        try {
            if (rescan) await RefreshExitNodes();
            exits = (await ListExitNodes()) || [];
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            exitsBusy = false;
        }
    }

    async function probeExit(id) {
        probing = id;
        error = "";
        notice = "";
        try {
            const r = await ProbeExitNode(id);
            notice = r.paymentRequired
                ? "Reachable. This exit asks for payment after its free test traffic."
                : r.ok
                  ? `Reachable: ${r.domainsOk} of ${r.domainsTried} test domains opened, about ${(r.measuredKbps / 1000).toFixed(1)} Mbit/s measured.`
                  : "Exit did not respond.";
            await refreshExits();
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            probing = "";
        }
    }

    async function loadDomains() {
        try { domainsText = ((await GetProbeDomains()) || []).join(", "); } catch (e) {}
    }

    async function saveDomains() {
        error = "";
        try {
            const list = domainsText.split(",").map((s) => s.trim()).filter(Boolean).map((s) => (s.includes(":") ? s : s + ":443"));
            await SetProbeDomains(list);
            await loadDomains();
            notice = "Test domains saved. Exits are re-checked against them automatically.";
        } catch (e) {
            error = String(e?.message || e);
        }
    }

    async function measureUpload() {
        shareBusy = true;
        error = "";
        try {
            const kbps = await MeasureUploadCapacity();
            form.uploadMbps = Math.round(kbps / 100) / 10;
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            shareBusy = false;
        }
    }

    async function toggleSharing() {
        shareBusy = true;
        error = "";
        try {
            if (sharing.enabled) {
                await StopSharing();
            } else {
                await StartSharing({
                    pricePerMb: microPerMb(form.price),
                    freeTestKb: Math.min(1024, Math.max(0, Math.round(Number(form.freeKb) || 0))),
                    uploadKbps: Math.round((Number(form.uploadMbps) || 0) * 1000),
                });
            }
            await refresh();
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            shareBusy = false;
        }
    }

    const fmtTime = (ts) => (ts ? new Date(ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "never");
    async function refresh() {
        try {
            vpn = await GetVPNStatus();
            sharing = await GetSharingStatus();
            if (!sharing.enabled) {
                form = { ...form, price: sharing.config.pricePerMb ? Number(usdPerGb(sharing.config.pricePerMb).toFixed(3)) : form.price, freeKb: sharing.config.freeTestKb || form.freeKb };
            }
            await refreshPay();
        } catch (e) {
            error = String(e?.message || e);
        }
    }

    async function refreshPay() {
        try {
            pay = await GetExitPayment();
            if (!payFormTouched) payForm = { chunk: pay.chunkUsdt || "1", cap: pay.dailyCapUsdt || "5" };
        } catch (e) {}
    }

    async function payNow(amount) {
        payBusy = true;
        error = "";
        payNotice = "";
        try {
            await PayExit(String(amount));
            payNotice = "Payment sent. It is credited when the network confirms it, usually within half a minute.";
            await refreshPay();
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            payBusy = false;
        }
    }

    async function saveAutoPay(enabled) {
        payBusy = true;
        error = "";
        try {
            await SetAutoPay(enabled, String(payForm.chunk), String(payForm.cap));
            payFormTouched = false;
            await refreshPay();
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            payBusy = false;
        }
    }

    async function allowAndPay() {
        await saveAutoPay(true);
        if (!error) await payNow(payForm.chunk);
    }

    async function toggleConnection() {
        busy = true;
        error = "";
        try {
            if (vpn.running) await StopVPN();
            else await StartVPN(selectedRelay);
            await refresh();
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            busy = false;
        }
    }

    async function toggleWholeComputer() {
        wholeBusy = true;
        error = "";
        try {
            await SetWholeComputer(!vpn.wholeComputer);
            await refresh();
        } catch (e) {
            error = String(e?.message || e);
        } finally {
            wholeBusy = false;
        }
    }

    function copyProxyAddress() {
        if (!vpn.listenAddr) return;
        navigator.clipboard.writeText(vpn.listenAddr);
        copiedProxy = true;
        setTimeout(() => (copiedProxy = false), 2000);
    }

    let poll;
    let payPoll;
    onMount(() => {
        refresh();
        refreshExits(true);
        loadDomains();
        poll = setInterval(() => { refresh(); refreshExits(); }, 15000);
        payPoll = setInterval(() => { if (vpn.running) refreshPay(); }, 5000);
        for (const t of ["vpn:started", "vpn:stopped", "exit:session_started", "exit:session_closed", "vpn:whole_computer", "vpn:payment", "exit:payment"]) {
            try { offs.push(EventsOn(t, refresh)); } catch (e) {}
        }
    });
    onDestroy(() => {
        clearInterval(poll);
        clearInterval(payPoll);
        offs.forEach((o) => typeof o === "function" && o());
    });
</script>

<div class="w-full h-full overflow-y-auto bg-background text-text">
    <div class="max-w-3xl w-full mx-auto px-5 py-6 space-y-6">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
            <div>
                <h1 class="text-xl font-extrabold">Private connection</h1>
                <p class="text-[15px] text-textMuted mt-0.5">Local proxy that routes application traffic through a relay</p>
            </div>
            <span class="inline-flex items-center gap-2 px-3 h-8 rounded-full border border-border text-sm font-bold self-start">
                <span class="w-2 h-2 rounded-full {vpn.running ? 'bg-success' : 'bg-textMuted'}"></span>
                <span>{vpn.running ? 'Proxy running' : 'Not running'}</span>
            </span>
        </div>

        <div class="flex flex-col items-center justify-center py-6">
            <button
                class="no-drag w-40 h-40 rounded-full flex flex-col items-center justify-center gap-2 transition-opacity hover:opacity-90 disabled:opacity-60 {vpn.running ? 'bg-success text-white' : 'bg-accent text-white'}"
                on:click={toggleConnection}
                disabled={busy || (!vpn.running && exits.length === 0)}
            >
                <span class="text-base font-extrabold">
                    {#if busy}Working...{:else if vpn.running}Turn off{:else}Turn on{/if}
                </span>
            </button>

            {#if error}
                <p class="mt-4 text-sm text-danger">{error}</p>
            {/if}

            <div class="mt-8 flex items-center gap-2">
                <label for="vpn-location" class="text-sm text-textMuted font-bold">Exit</label>
                {#if exits.length}
                    <select id="vpn-location" bind:value={selectedRelay} disabled={vpn.running} class="no-drag ef-input !w-auto !py-2 !text-sm bg-background">
                        <option value="auto">Best rated (automatic)</option>
                        {#each exits as x}
                            <option value={x.peerId}>{x.peerId.slice(0, 12)} ({fmtPrice(x.pricePerMb)}, {(x.claimedKbps / 1000).toFixed(0)} Mbit/s)</option>
                        {/each}
                    </select>
                {:else}
                    <span class="text-sm text-textMuted">No exit nodes discovered yet</span>
                {/if}
            </div>
        </div>

        {#if vpn.running && !vpn.tunnelReady}
            <div class="ef-card p-4 text-sm text-textMuted">
                The local proxy is listening, but traffic cannot be forwarded yet: {vpn.tunnelNotice}.
            </div>
        {/if}

        {#if vpn.running && pay.active && pay.canPay}
            <div class="ef-card p-5 space-y-3">
                <div class="flex items-start justify-between gap-3">
                    <div>
                        <div class="text-[15px] font-bold">Traffic on this exit</div>
                        <p class="text-sm text-textMuted mt-0.5">{pay.pricePerGb} USDT per GB. {pay.remainingGb > 0 ? pay.remainingGb.toFixed(1) + " GB left" : "No traffic left"}.</p>
                    </div>
                    {#if pay.pending}<span class="text-xs font-bold px-2.5 py-1 rounded-full bg-[#f0b232] text-black">Payment confirming</span>{/if}
                </div>

                {#if pay.needsPayment}
                    <div class="rounded-2xl bg-surface p-4">
                        <p class="text-sm"><b>Add {pay.chunkUsdt} USDT</b> for about {pay.chunkGb.toFixed(0)} GB. Paid from your EchoFog wallet to the exit's owner. This exit's free test is used up.</p>
                        <div class="flex flex-wrap gap-2 mt-3">
                            <button class="no-drag ef-btn ef-btn-accent" disabled={payBusy} on:click={() => payNow(payForm.chunk)}>{payBusy ? "Paying..." : "Pay " + payForm.chunk + " USDT"}</button>
                            <button class="no-drag ef-btn ef-btn-ghost" disabled={payBusy} on:click={allowAndPay}>Pay and top up automatically</button>
                        </div>
                    </div>
                {/if}

                <div class="flex flex-col sm:flex-row sm:items-end gap-3">
                    <label class="text-sm text-textMuted flex-1">Top-up, USDT
                        <input type="number" min="0.01" step="0.5" class="ef-input mt-1" bind:value={payForm.chunk} on:input={() => (payFormTouched = true)} />
                    </label>
                    <label class="text-sm text-textMuted flex-1">Daily limit, USDT
                        <input type="number" min="0.01" step="1" class="ef-input mt-1" bind:value={payForm.cap} on:input={() => (payFormTouched = true)} />
                    </label>
                    <button class="no-drag ef-btn {pay.autoPay ? 'ef-btn-danger' : 'ef-btn-accent'} shrink-0" disabled={payBusy} on:click={() => saveAutoPay(!pay.autoPay)}>
                        {pay.autoPay ? "Stop automatic top-ups" : "Top up automatically"}
                    </button>
                </div>
                <p class="text-xs text-textMuted">
                    {#if pay.autoPay}Automatic top-ups are on: when only a few GB is left, {pay.chunkUsdt} USDT is paid, at most {pay.dailyCapUsdt} USDT a day. Paid today: {pay.spentTodayUsdt} USDT.{:else}Automatic top-ups are off: you are asked before every payment. Nothing is paid without your say.{/if}
                    Payments need USDT and about 0.05 TON for the network fee in your wallet.
                </p>
                {#if pay.note}<p class="text-sm text-textMuted">{pay.note}.</p>{/if}
                {#if payNotice}<p class="text-sm text-success">{payNotice}</p>{/if}
                {#if pay.lastError}<p class="text-sm text-danger">{pay.lastError}</p>{/if}
            </div>
        {/if}

        {#if vpn.wholeComputerSupported}
            <div class="ef-card p-5 flex flex-col sm:flex-row sm:items-center justify-between gap-4">
                <div class="min-w-0">
                    <div class="text-[15px] font-bold">Whole computer</div>
                    <p class="text-sm text-textMuted mt-0.5">
                        {#if vpn.wholeComputer}
                            Every app on this computer goes through the exit, including games, calls and DNS.
                        {:else}
                            Send every app through the exit, not only those set up to use the proxy. Asks for administrator permission.
                        {/if}
                    </p>
                    {#if vpn.wholeComputerError && !vpn.wholeComputer}
                        <p class="text-sm text-danger mt-1">Stopped: {vpn.wholeComputerError}</p>
                    {/if}
                </div>
                <button
                    class="no-drag ef-btn shrink-0 {vpn.wholeComputer ? 'ef-btn-danger' : 'ef-btn-accent'}"
                    disabled={wholeBusy || !vpn.running}
                    title={vpn.running ? "" : "Turn the connection on first"}
                    on:click={toggleWholeComputer}
                >
                    {#if wholeBusy}{vpn.wholeComputer ? "Turning off..." : "Waiting for permission..."}{:else if vpn.wholeComputer}Turn off{:else}Turn on{/if}
                </button>
            </div>
        {/if}

        <div class="grid grid-cols-2 md:grid-cols-4 gap-3">
            <div class="ef-card p-4">
                <div class="text-sm text-textMuted mb-1">Received</div>
                <div class="text-xl font-extrabold">{fmtBytes(vpn.bytesRx)}</div>
            </div>
            <div class="ef-card p-4">
                <div class="text-sm text-textMuted mb-1">Sent</div>
                <div class="text-xl font-extrabold">{fmtBytes(vpn.bytesTx)}</div>
            </div>
            <div class="ef-card p-4">
                <div class="text-sm text-textMuted mb-1">Active connections</div>
                <div class="text-xl font-extrabold">{vpn.activeConns}</div>
            </div>
            <div class="ef-card p-4">
                <div class="text-sm text-textMuted mb-1">Exit nodes known</div>
                <div class="text-xl font-extrabold text-accent">{exits.length}</div>
            </div>
        </div>

        <div class="ef-card p-5">
            <div class="flex items-center justify-between mb-3">
                <div class="text-[15px] font-bold">Exit nodes</div>
                <button class="no-drag ef-btn ef-btn-ghost !h-8 !px-3 text-sm" disabled={exitsBusy} on:click={() => refreshExits(true)}>{exitsBusy ? "Scanning..." : "Rescan"}</button>
            </div>
            {#if notice}<p class="text-sm text-textMuted mb-3">{notice}</p>{/if}
            <div class="flex gap-2 mb-3">
                <input class="ef-input !h-9 text-sm" placeholder="Sites to test through each exit, e.g. example.org, news.site:443" aria-label="Test domains" bind:value={domainsText} />
                <button class="no-drag ef-btn ef-btn-ghost !h-9 !px-3 text-sm shrink-0" on:click={saveDomains}>Save</button>
            </div>
            {#if exits.length}
                <div class="space-y-1">
                    {#each exits as x}
                        <div class="ef-row flex items-center justify-between gap-3 text-sm py-2">
                            <div class="min-w-0">
                                <div class="font-mono truncate">{x.peerId.slice(0, 16)}{x.connected ? "" : " (not connected)"}</div>
                                <div class="text-textMuted">
                                    {fmtPrice(x.pricePerMb)} · claims {(x.claimedKbps / 1000).toFixed(1)} Mbit/s
                                    {#if x.measuredKbps} · measured {(x.measuredKbps / 1000).toFixed(1)}{/if}
                                    {#if x.latencyMs} · {Math.round(x.latencyMs)} ms{/if}
                                    {#if x.domainsTried} · sites {x.domainsOk}/{x.domainsTried}{/if}
                                    · free test {x.freeTestKb} KB
                                    {#if x.probes} · checked {x.probes}x, {x.successes} ok{:else} · not checked yet{/if}
                                </div>
                            </div>
                            <div class="flex items-center gap-2 shrink-0">
                                <span class="font-bold {x.score >= 0.5 ? 'text-success' : 'text-textMuted'}">{(x.score * 100).toFixed(0)}</span>
                                <button class="no-drag ef-btn ef-btn-ghost !h-8 !px-3 text-xs" disabled={probing === x.peerId} on:click={() => probeExit(x.peerId)}>{probing === x.peerId ? "Testing..." : "Test"}</button>
                            </div>
                        </div>
                    {/each}
                </div>
            {:else}
                <p class="text-sm text-textMuted">Nothing found yet. Exits appear here once the node meets peers that share their connection.</p>
            {/if}
        </div>

        <div class="ef-card p-5">
            <div class="text-[15px] font-bold">Share my connection</div>
            <p class="text-sm text-textMuted mt-0.5">
                Publish this computer as an exit in the network. Other users reach the internet through you and their sites see your IP address. Local and private network addresses are always blocked. Sharing turns off whenever the app restarts.
            </p>

            {#if !sharing.enabled}
                <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mt-4">
                    <label class="text-sm text-textMuted">Price per GB, USDT (0 = free)
                        <input type="number" min="0" step="0.01" class="ef-input mt-1" bind:value={form.price} />
                    </label>
                    <label class="text-sm text-textMuted">Free test traffic, KB (max 1024)
                        <input type="number" min="0" max="1024" class="ef-input mt-1" bind:value={form.freeKb} />
                    </label>
                    <label class="text-sm text-textMuted">Upload speed, Mbit/s
                        <div class="flex gap-2 mt-1">
                            <input type="number" min="0" step="0.1" class="ef-input" bind:value={form.uploadMbps} />
                            <button class="no-drag ef-btn ef-btn-ghost !px-3 text-xs shrink-0" disabled={shareBusy} on:click={measureUpload}>Measure</button>
                        </div>
                    </label>
                </div>
            {:else}
                <div class="mt-3 text-sm text-textMuted space-y-1">
                    <p>Published at {fmtTime(sharing.lastPublish)}, valid until {fmtTime(sharing.expiresAt)}. It refreshes automatically.</p>
                    <p>Price {fmtPrice(sharing.config.pricePerMb)}, free test {sharing.config.freeTestKb} KB, speed {(sharing.config.uploadKbps / 1000).toFixed(1)} Mbit/s</p>
                    <p>{sharing.accepted} served, {sharing.refused} refused, {fmtBytes(sharing.bytes)} relayed</p>
                    {#if sharing.config.pricePerMb > 0}
                        <p>Earned from clients: <b class="text-text">{sharing.earnedUsdt} USDT</b>. It lands in your EchoFog wallet.{#if !sharing.paymentsReady} <span class="text-danger">Not ready to receive payments yet: open the wallet and log in.</span>{/if}</p>
                    {/if}
                    {#if sharing.lastError}<p class="text-danger">Last publish failed: {sharing.lastError}</p>{/if}
                </div>
            {/if}

            <div class="mt-4">
                <button class="no-drag ef-btn {sharing.enabled ? 'ef-btn-danger' : 'ef-btn-accent'}" disabled={shareBusy} on:click={toggleSharing}>
                    {shareBusy ? "Working..." : sharing.enabled ? "Stop sharing" : "Start sharing"}
                </button>
            </div>
        </div>

        <div class="ef-card p-5">
            <button
                class="no-drag w-full flex items-center justify-between text-[15px] font-bold"
                on:click={() => (showDetails = !showDetails)}
            >
                <span>Details</span>
                <svg class="w-5 h-5 text-textMuted transition-transform {showDetails ? 'rotate-90' : ''}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M9 6l6 6-6 6"/></svg>
            </button>
            {#if showDetails}
                <div class="mt-4 pt-4 border-t border-border flex flex-col sm:flex-row sm:items-center justify-between gap-4">
                    <div class="text-sm text-textMuted">
                        {#if vpn.running}
                            Use this SOCKS5 address in application proxy settings:
                            <code class="font-mono font-bold text-text">{vpn.listenAddr}</code>
                        {:else}
                            The proxy address is available once the connection is turned on.
                        {/if}
                    </div>
                    <button class="no-drag ef-btn ef-btn-ghost shrink-0" on:click={copyProxyAddress} disabled={!vpn.running}>
                        {copiedProxy ? 'Copied' : 'Copy address'}
                    </button>
                </div>
            {/if}
        </div>
    </div>
</div>
