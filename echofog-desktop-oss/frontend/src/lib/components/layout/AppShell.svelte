<script>
    import { AVAILABLE_APPS, activeTabId, openApp } from "../../stores/layout.store.js";
    import { currentTheme, toggleTheme } from "../../stores/theme.store.js";
    import Workspace from "./Workspace.svelte";
    import CallOverlay from "../ui/CallOverlay.svelte";

    // Launch scope: communication (chats with calls, contacts), the proxy and its wallet.
    const PRIMARY = ["chat", "contacts", "vpn", "wallet", "logs"];
    const byId = (id) => AVAILABLE_APPS.find((a) => a.id === id);

    function go(id) {
        openApp(id, byId(id)?.title ?? id);
    }
</script>

<div class="flex w-full h-full bg-background text-text">
    <nav class="w-[68px] shrink-0 border-r border-border flex flex-col items-center py-3 gap-1 no-drag" aria-label="Main">
        <div class="w-10 h-10 rounded-full bg-accent text-white font-extrabold flex items-center justify-center mb-3 select-none">E</div>

        {#each PRIMARY as id}
            {@const app = byId(id)}
            <button
                class="ef-icon-btn !w-11 !h-11 {$activeTabId === id ? '!text-text !bg-[var(--color-hover)]' : ''}"
                title={app.label}
                aria-label={app.label}
                on:click={() => go(id)}
            >
                <svg class="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width={$activeTabId === id ? 2.4 : 1.8} stroke-linecap="round" stroke-linejoin="round"><path d={app.icon} /></svg>
            </button>
        {/each}

        <div class="flex-1"></div>

        <button class="ef-icon-btn !w-11 !h-11" title="Switch theme" aria-label="Switch theme" on:click={toggleTheme}>
            {#if $currentTheme === "super-light"}
                <svg class="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20.35 15.35A9 9 0 018.65 3.65 9 9 0 1020.35 15.35z"/></svg>
            {:else}
                <svg class="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>
            {/if}
        </button>
        <button class="ef-icon-btn !w-11 !h-11 {$activeTabId === 'settings' ? '!text-text !bg-[var(--color-hover)]' : ''}" title="Settings" aria-label="Settings" on:click={() => go("settings")}>
            <svg class="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d={byId("settings").icon} /></svg>
        </button>
    </nav>

    <main class="flex-1 min-w-0 min-h-0 relative">
        <div class="absolute top-0 left-0 right-0 h-3 z-10" style="--wails-draggable:drag;"></div>
        <Workspace />
    </main>
</div>

<CallOverlay />

