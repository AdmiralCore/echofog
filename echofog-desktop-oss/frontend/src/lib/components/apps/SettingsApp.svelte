<script>
    import { currentTheme, toggleTheme } from "../../stores/theme.store.js";
    import { onMount } from "svelte";
    import { GetSettings, SetSettings } from "@wailsjs/go/main/App";

    let config = null;
    let backendError = "";
    let showDetails = false;
    let isSaved = false;

    function errText(e) {
        return (e && (e.message || String(e))) || "Request failed";
    }

    onMount(async () => {
        try {
            config = await GetSettings();
        } catch (e) {
            backendError = errText(e);
        }
    });

    function setTheme(t) {
        if ($currentTheme !== t) toggleTheme();
    }

    async function saveConfig() {
        backendError = "";
        try {
            if (config) await SetSettings(config);
            isSaved = true;
            setTimeout(() => (isSaved = false), 2500);
        } catch (e) {
            backendError = errText(e);
        }
    }
</script>

<div class="w-full h-full overflow-y-auto bg-background text-text">
    <div class="max-w-3xl w-full mx-auto px-5 py-6 space-y-6">
        <div class="flex items-center justify-between gap-4">
            <div>
                <h1 class="text-xl font-extrabold">Settings</h1>
                <p class="text-[15px] text-textMuted mt-0.5">Appearance and connection</p>
            </div>
            <button class="no-drag ef-btn ef-btn-accent" on:click={saveConfig}>
                {isSaved ? 'Saved' : 'Save changes'}
            </button>
        </div>

        {#if backendError}
            <div class="ef-card p-4 text-sm text-danger">{backendError}</div>
        {/if}

        <!-- Appearance -->
        <div class="ef-card p-5 flex items-center justify-between gap-4">
            <div>
                <h2 class="text-[15px] font-bold">Appearance</h2>
                <p class="text-sm text-textMuted">Choose a dark or light look</p>
            </div>
            <div class="inline-flex rounded-full border border-border p-1 bg-background">
                <button
                    class="no-drag px-4 h-8 rounded-full text-sm font-bold transition-colors {$currentTheme === 'super-dark' ? 'bg-accent text-white' : 'text-textMuted hover:text-text'}"
                    on:click={() => setTheme('super-dark')}
                >Dark</button>
                <button
                    class="no-drag px-4 h-8 rounded-full text-sm font-bold transition-colors {$currentTheme === 'super-light' ? 'bg-accent text-white' : 'text-textMuted hover:text-text'}"
                    on:click={() => setTheme('super-light')}
                >Light</button>
            </div>
        </div>

        <!-- Connection and privacy -->
        <div class="ef-card p-5 space-y-1">
            <h2 class="text-[15px] font-bold mb-2">Connection and privacy</h2>
            {#if !config}
                <p class="text-sm text-textMuted">Connection options are available once your node is running.</p>
            {:else}

            <label class="flex items-center justify-between gap-4 py-3 border-t border-border">
                <div>
                    <div class="text-[15px] font-bold">Find people nearby</div>
                    <div class="text-sm text-textMuted">See other EchoFog users on your Wi-Fi, even without internet.</div>
                </div>
                <input type="checkbox" bind:checked={config.enableMdns} class="toggle toggle-sm no-drag" />
            </label>

            <label class="flex items-center justify-between gap-4 py-3 border-t border-border">
                <div>
                    <div class="text-[15px] font-bold">Hide my connection</div>
                    <div class="text-sm text-textMuted">Makes your traffic look like ordinary web browsing, so it is harder to block.</div>
                </div>
                <input type="checkbox" bind:checked={config.antiDpi} class="toggle toggle-sm no-drag" />
            </label>

            <label class="flex items-center justify-between gap-4 py-3 border-t border-border">
                <div>
                    <div class="text-[15px] font-bold">Stay anonymous</div>
                    <div class="text-sm text-textMuted">Do not show your name to strangers. People can only reach you with your ID.</div>
                </div>
                <input type="checkbox" bind:checked={config.anonymousMode} class="toggle toggle-sm no-drag" />
            </label>

            <label class="flex items-center justify-between gap-4 py-3 border-t border-border">
                <div>
                    <div class="text-[15px] font-bold">Fast mode</div>
                    <div class="text-sm text-textMuted">Uses your network card more directly for the best speed on fast connections.</div>
                </div>
                <input type="checkbox" bind:checked={config.directSocket} class="toggle toggle-sm no-drag" />
            </label>

            <!-- Details -->
            <div class="border-t border-border pt-3">
                <button
                    class="no-drag flex items-center gap-1.5 text-sm font-bold text-textMuted hover:text-text"
                    on:click={() => (showDetails = !showDetails)}
                >
                    <svg class="w-4 h-4 transition-transform {showDetails ? 'rotate-90' : ''}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M9 6l6 6-6 6"/></svg>
                    Details
                </button>
                {#if showDetails}
                    <div class="mt-3">
                        <div class="flex justify-between text-sm mb-1 font-bold">
                            <span>Network buffer size</span>
                            <span class="text-textMuted">{config.socketBufferMb} MB</span>
                        </div>
                        <input
                            type="range"
                            min="4"
                            max="64"
                            step="4"
                            bind:value={config.socketBufferMb}
                            class="range range-xs no-drag w-full"
                        />
                        <div class="flex justify-between text-xs text-textMuted mt-1">
                            <span>4 MB (phone)</span>
                            <span>32 MB (standard)</span>
                            <span>64 MB (server)</span>
                        </div>
                    </div>
                {/if}
            </div>
            <p class="text-xs text-textMuted pt-2">Connection options take effect the next time your node starts.</p>
            {/if}
        </div>

    </div>
</div>
