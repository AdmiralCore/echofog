<script>
    import { createEventDispatcher, onMount } from "svelte";
    import { fade, fly } from "svelte/transition";

    const dispatch = createEventDispatcher();

    let step = 0;
    let words = new Array(12).fill("");
    let currentWord = "";
    let inputEl;

    let mnemonicPassphrase = "";
    let appPassword = "";

    let showMnemonic = true;
    let showMnemonicPassphrase = false;
    let showAppPassword = false;

    let isRecovering = false;
    let errorMsg = "";

    $: currentWord = currentWord.toLowerCase().replace(/[^a-z]/g, "");

    onMount(() => {
        setTimeout(() => inputEl && inputEl.focus(), 200);
    });

    function focusInput() {
        if (inputEl) inputEl.focus();
    }

    function nextWord() {
        if (!currentWord.trim()) return;

        words[step] = currentWord.trim();
        currentWord = "";

        // Find next empty word or go to finish step
        const nextEmptyIndex = words.findIndex((w, i) => i > step && !w);
        if (nextEmptyIndex !== -1) {
            step = nextEmptyIndex;
            setTimeout(focusInput, 50);
        } else if (words.every((w) => w)) {
            step = 12;
        } else {
            // Find any empty index
            const anyEmpty = words.indexOf("");
            if (anyEmpty !== -1) {
                step = anyEmpty;
                setTimeout(focusInput, 50);
            }
        }
    }

    function selectStep(i) {
        if (isRecovering) return;

        // Save current work if moving from an active input
        if (step < 12 && currentWord.trim()) {
            words[step] = currentWord.trim();
        }

        step = i;
        currentWord = words[i] || "";
        setTimeout(focusInput, 50);
    }

    function handleKeydown(e) {
        if (e.key === "Enter") nextWord();
    }

    async function finish() {
        if (!appPassword || appPassword.length < 4) {
            errorMsg = "Password must be at least 4 characters.";
            return;
        }

        isRecovering = true;
        errorMsg = "";
        const mnemonicString = words.join(" ");

        try {
            await window.go.main.App.Recover(
                mnemonicString,
                mnemonicPassphrase,
                appPassword,
            );

            dispatch("complete");
        } catch (err) {
            console.error("Failed to recover:", err);
            errorMsg = "We couldn't restore your account: " + err;
        } finally {
            isRecovering = false;
        }
    }

    function handleImport() {
        throw new Error("Function not implemented.");
    }
</script>

<div class="w-full h-full flex flex-col items-center justify-center p-4 relative z-10" in:fade>
    <div class="w-full max-w-md">
        <button
            class="no-drag ef-btn ef-btn-ghost mb-6"
            on:click={() => dispatch("cancel")}
            disabled={isRecovering}
        >
            ← Back
        </button>

        <h2 class="text-2xl font-extrabold text-center mb-2 select-none">Restore your account</h2>
        <p class="text-sm text-textMuted text-center mb-6">Enter your 12 recovery words, one at a time, in the order you saved them.</p>

        <div class="min-h-[160px] flex flex-col items-center justify-center">
            {#if step < 12}
                <div in:fly={{ y: 12 }} class="w-full space-y-4">
                    <label for="mnemonic-input" class="text-sm font-semibold block">
                        Word {step + 1} of 12
                    </label>

                    <div class="relative w-full">
                        {#if showMnemonic}
                            <input
                                id="mnemonic-input"
                                type="text"
                                bind:this={inputEl}
                                bind:value={currentWord}
                                on:keydown={handleKeydown}
                                class="no-drag ef-input text-xl text-center py-4 pr-12"
                                placeholder="English word"
                                autocomplete="off"
                                spellcheck="false"
                            />
                        {:else}
                            <input
                                id="mnemonic-input"
                                type="password"
                                bind:this={inputEl}
                                bind:value={currentWord}
                                on:keydown={handleKeydown}
                                class="no-drag ef-input text-xl text-center py-4 pr-12"
                                placeholder="English word"
                                autocomplete="off"
                                spellcheck="false"
                            />
                        {/if}
                        <button
                            class="no-drag absolute inset-y-0 right-2 ef-icon-btn my-auto"
                            on:click={() => (showMnemonic = !showMnemonic)}
                            tabindex="-1"
                            title={showMnemonic ? "Hide words" : "Show words"}
                            aria-label={showMnemonic ? "Hide words" : "Show words"}
                        >
                            <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
                                {#if showMnemonic}<path d="M2 12s3.5-7.5 10-7.5S22 12 22 12s-3.5 7.5-10 7.5S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>{:else}<path d="M3 3l18 18M10.6 6.1A10 10 0 0122 12a10 10 0 01-3.2 4.4M6.2 6.2A10 10 0 002 12c1.3 4.3 5.3 7.5 10 7.5 1 0 1.9-.1 2.9-.4"/>{/if}
                            </svg>
                        </button>
                    </div>

                    <button class="no-drag ef-btn ef-btn-accent w-full h-12" on:click={nextWord}>
                        {step === 11 ? "Done" : "Next word"}
                    </button>
                </div>
            {:else}
                <div in:fade class="w-full flex flex-col gap-4">
                    <div class="flex items-center justify-center gap-2 text-success font-bold">
                        <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12l5 5L20 7"/></svg>
                        All 12 words entered
                    </div>

                    <div>
                        <label for="app-pass-input" class="block text-sm font-semibold mb-2">New password</label>
                        <div class="relative w-full">
                            {#if showAppPassword}
                                <input
                                    id="app-pass-input"
                                    type="text"
                                    bind:value={appPassword}
                                    class="no-drag ef-input pr-11"
                                    placeholder="Choose a password for this device"
                                    on:keydown={(e) => e.key === "Enter" && handleImport()}
                                />
                            {:else}
                                <input
                                    id="app-pass-input"
                                    type="password"
                                    bind:value={appPassword}
                                    class="no-drag ef-input pr-11"
                                    placeholder="Choose a password for this device"
                                    on:keydown={(e) => e.key === "Enter" && handleImport()}
                                />
                            {/if}
                            <button
                                class="no-drag absolute inset-y-0 right-2 ef-icon-btn my-auto"
                                on:click={() => (showAppPassword = !showAppPassword)}
                                tabindex="-1"
                                aria-label="Show password"
                            >
                                <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
                                    {#if showAppPassword}<path d="M3 3l18 18M10.6 6.1A10 10 0 0122 12a10 10 0 01-3.2 4.4M6.2 6.2A10 10 0 002 12c1.3 4.3 5.3 7.5 10 7.5 1 0 1.9-.1 2.9-.4"/>{:else}<path d="M2 12s3.5-7.5 10-7.5S22 12 22 12s-3.5 7.5-10 7.5S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>{/if}
                                </svg>
                            </button>
                        </div>
                    </div>

                    <details class="text-sm">
                        <summary class="cursor-pointer text-textMuted">Details (extra secret word)</summary>
                        <div class="mt-3">
                            <label for="mnemonic-pass-input" class="block text-sm font-semibold mb-1">Extra secret word</label>
                            <p class="text-sm text-textMuted mb-2">Only fill this in if you added one when you created your account.</p>
                            <div class="relative w-full">
                                {#if showMnemonicPassphrase}
                                    <input id="mnemonic-pass-input" type="text" bind:value={mnemonicPassphrase} class="no-drag ef-input pr-11" placeholder="Leave empty if you didn't set one" />
                                {:else}
                                    <input id="mnemonic-pass-input" type="password" bind:value={mnemonicPassphrase} class="no-drag ef-input pr-11" placeholder="Leave empty if you didn't set one" />
                                {/if}
                                <button
                                    class="no-drag absolute inset-y-0 right-2 ef-icon-btn my-auto"
                                    on:click={() => (showMnemonicPassphrase = !showMnemonicPassphrase)}
                                    tabindex="-1"
                                    aria-label="Show extra word"
                                >
                                    <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
                                        {#if showMnemonicPassphrase}<path d="M3 3l18 18M10.6 6.1A10 10 0 0122 12a10 10 0 01-3.2 4.4M6.2 6.2A10 10 0 002 12c1.3 4.3 5.3 7.5 10 7.5 1 0 1.9-.1 2.9-.4"/>{:else}<path d="M2 12s3.5-7.5 10-7.5S22 12 22 12s-3.5 7.5-10 7.5S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>{/if}
                                    </svg>
                                </button>
                            </div>
                        </div>
                    </details>

                    {#if errorMsg}
                        <p class="text-danger text-sm text-center">{errorMsg}</p>
                    {/if}

                    <button class="no-drag ef-btn ef-btn-accent w-full h-12" on:click={finish} disabled={isRecovering}>
                        {#if isRecovering}
                            <span class="w-4 h-4 rounded-full border-2 border-current border-t-transparent animate-spin"></span>
                            Restoring…
                        {:else}
                            Restore account
                        {/if}
                    </button>
                </div>
            {/if}
        </div>

        <div class="grid grid-cols-4 gap-2 mt-8 pt-6 border-t border-border">
            {#each words as word, i}
                <button
                    class="no-drag text-xs py-2 px-1 rounded-lg text-center transition-colors border
                    {i === step
                        ? 'border-accent text-accent'
                        : word
                          ? 'border-border text-text hover:bg-hover'
                          : 'border-border text-textMuted hover:bg-hover'}"
                    on:click={() => selectStep(i)}
                    disabled={isRecovering}
                    title={word ? "Click to edit" : "Click to select"}
                >
                    <div class="flex flex-col items-center">
                        <span class="text-[10px] text-textMuted mb-0.5">{i + 1}</span>
                        <span class="truncate w-full inline-block">
                            {word ? (showMnemonic ? word : "••••") : "—"}
                        </span>
                    </div>
                </button>
            {/each}
        </div>
    </div>
</div>
