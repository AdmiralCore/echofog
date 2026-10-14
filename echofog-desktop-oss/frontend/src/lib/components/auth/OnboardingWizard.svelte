<script>
    import { createEventDispatcher } from "svelte";
    import { fly } from "svelte/transition";

    const dispatch = createEventDispatcher();

    let step = 1;

    // Preferences selected during wizard
    let selectedGoal = "surfer"; // 'surfer' | 'earner' | 'stealth'
    let enableAI = true;
    let mcpAccessLevel = "read_write"; // 'read_only' | 'read_write' | 'disabled'
    let nodeMode = "standard"; // 'standard' | 'advanced'

    // Key creation state
    let appPassword = "";
    let confirmPassword = "";
    let mnemonicPassphrase = "";
    let showPassword = false;
    let isGenerating = false;
    let errorMsg = "";
    let mnemonicWords = [];
    let copiedMnemonic = false;

    function nextStep() {
        errorMsg = "";
        if (step === 3 && nodeMode === "standard") {
            // Quick skip advanced settings
            step = 4;
            return;
        }
        step++;
    }

    function prevStep() {
        errorMsg = "";
        if (step > 1) step--;
        else dispatch("cancel");
    }

    async function handleGenerateKeys() {
        if (!appPassword || appPassword.length < 4) {
            errorMsg = "Password must be at least 4 characters.";
            return;
        }
        if (appPassword !== confirmPassword) {
            errorMsg = "Passwords do not match.";
            return;
        }

        isGenerating = true;
        errorMsg = "";

        try {
            const resultMnemonic = await window.go.main.App.Register(
                appPassword,
                mnemonicPassphrase
            );
            mnemonicWords = resultMnemonic.trim().split(" ");
            step = 5; // Move to recovery words confirmation
        } catch (err) {
            console.error("Registration error:", err);
            errorMsg = "We couldn't create your account: " + err;
        } finally {
            isGenerating = false;
        }
    }

    function copySeedPhrase() {
        navigator.clipboard.writeText(mnemonicWords.join(" "));
        copiedMnemonic = true;
        setTimeout(() => (copiedMnemonic = false), 2500);
    }

    function completeOnboarding() {
        dispatch("complete", {
            goal: selectedGoal,
            enableAI,
            mcpAccessLevel,
            nodeMode
        });
    }

    const goals = [
        { id: "surfer", title: "Everyday use", badge: "Recommended", desc: "Chat privately, open blocked websites and send files. Uses very little of your connection." },
        { id: "earner", title: "Help the network", badge: "Earn rewards", desc: "Share some of your spare internet connection to help others and earn points." },
        { id: "stealth", title: "Extra privacy", badge: "Strongest", desc: "Hides that you are using EchoFog. Best if your internet is heavily restricted or monitored." },
    ];
</script>

<div class="w-full max-w-lg mx-auto p-4">
    <!-- Step indicator -->
    <div class="flex items-center justify-between mb-8">
        <span class="text-sm text-textMuted">Step {step} of 5</span>
        <div class="flex items-center gap-1.5">
            {#each [1, 2, 3, 4, 5] as s}
                <div class="h-1.5 rounded-full transition-all {s === step ? 'w-6 bg-accent' : s < step ? 'w-2 bg-accent' : 'w-2 bg-border'}"></div>
            {/each}
        </div>
    </div>

    {#if step === 1}
        <div in:fly={{ y: 12, duration: 200 }} class="text-center">
            <div class="w-12 h-12 rounded-full bg-accent mx-auto mb-6 flex items-center justify-center text-white font-bold text-xl">E</div>
            <h2 class="text-2xl font-extrabold mb-3">Welcome to EchoFog</h2>
            <p class="text-[15px] text-textMuted mb-8">
                Private messaging that works without big companies in the middle. Let's set up your account in a few quick steps.
            </p>

            <div class="space-y-3 text-left mb-8">
                <div class="ef-card p-4 flex items-start gap-3">
                    <svg class="w-6 h-6 shrink-0 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3l8 3v6c0 4.5-3.2 8-8 9-4.8-1-8-4.5-8-9V6l8-3z"/></svg>
                    <div>
                        <div class="font-bold text-[15px]">Works where others are blocked</div>
                        <div class="text-sm text-textMuted">Your traffic looks like ordinary web browsing.</div>
                    </div>
                </div>
                <div class="ef-card p-4 flex items-start gap-3">
                    <svg class="w-6 h-6 shrink-0 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 018 0v3"/></svg>
                    <div>
                        <div class="font-bold text-[15px]">Only you can read your messages</div>
                        <div class="text-sm text-textMuted">Everything is locked on your device before it is sent.</div>
                    </div>
                </div>
                <div class="ef-card p-4 flex items-start gap-3">
                    <svg class="w-6 h-6 shrink-0 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2L4 14h7l-1 8 9-12h-7l1-8z"/></svg>
                    <div>
                        <div class="font-bold text-[15px]">Fast and direct</div>
                        <div class="text-sm text-textMuted">Your device talks straight to other people, with no middleman.</div>
                    </div>
                </div>
            </div>

            <button class="no-drag ef-btn ef-btn-accent w-full h-12" on:click={nextStep}>Get started</button>
        </div>

    {:else if step === 2}
        <div in:fly={{ y: 12, duration: 200 }}>
            <h2 class="text-2xl font-extrabold mb-1">How will you use EchoFog?</h2>
            <p class="text-sm text-textMuted mb-6">Pick what fits you best. You can change this later in settings.</p>

            <div class="space-y-3">
                {#each goals as g}
                    <button
                        type="button"
                        class="no-drag w-full text-left p-4 rounded-2xl border transition-colors {selectedGoal === g.id ? 'border-accent bg-surface' : 'border-border hover:bg-surface'}"
                        on:click={() => (selectedGoal = g.id)}
                    >
                        <div class="flex items-center justify-between mb-1">
                            <span class="font-bold text-[15px]">{g.title}</span>
                            <span class="text-xs font-semibold px-2 py-0.5 rounded-full border border-border text-textMuted">{g.badge}</span>
                        </div>
                        <p class="text-sm text-textMuted">{g.desc}</p>
                    </button>
                {/each}
            </div>

            <div class="flex gap-3 pt-6">
                <button class="no-drag ef-btn ef-btn-ghost" on:click={prevStep}>Back</button>
                <button class="no-drag ef-btn ef-btn-accent flex-1" on:click={nextStep}>Continue</button>
            </div>
        </div>

    {:else if step === 3}
        <div in:fly={{ y: 12, duration: 200 }}>
            <h2 class="text-2xl font-extrabold mb-1">AI assistants</h2>
            <p class="text-sm text-textMuted mb-6">
                AI assistant apps can help you manage EchoFog. You decide how much they are allowed to do.
            </p>

            <div class="space-y-4">
                <div class="ef-card p-4 flex items-center justify-between gap-4">
                    <div>
                        <div class="font-bold text-[15px]">Allow AI assistants</div>
                        <div class="text-sm text-textMuted">Let AI apps on this computer talk to EchoFog.</div>
                    </div>
                    <input type="checkbox" bind:checked={enableAI} class="toggle toggle-accent no-drag" />
                </div>

                {#if enableAI}
                    <div in:fly={{ y: 8, duration: 150 }} class="ef-card p-4">
                        <div class="font-bold text-[15px] mb-3">What can they do?</div>
                        <div class="grid grid-cols-2 gap-3">
                            <button
                                type="button"
                                class="no-drag p-3 rounded-2xl border text-left transition-colors {mcpAccessLevel === 'read_only' ? 'border-accent' : 'border-border hover:bg-hover'}"
                                on:click={() => (mcpAccessLevel = 'read_only')}
                            >
                                <div class="font-bold text-sm mb-0.5">View only</div>
                                <div class="text-sm text-textMuted">Can see how things are going, but can't change anything.</div>
                            </button>
                            <button
                                type="button"
                                class="no-drag p-3 rounded-2xl border text-left transition-colors {mcpAccessLevel === 'read_write' ? 'border-accent' : 'border-border hover:bg-hover'}"
                                on:click={() => (mcpAccessLevel = 'read_write')}
                            >
                                <div class="font-bold text-sm mb-0.5">View and change</div>
                                <div class="text-sm text-textMuted">Can also adjust settings to keep things fast.</div>
                            </button>
                        </div>
                    </div>
                {/if}

                <div class="ef-card p-4">
                    <div class="font-bold text-[15px] mb-3">Setup type</div>
                    <div class="grid grid-cols-2 gap-3">
                        <button
                            type="button"
                            class="no-drag p-3 rounded-full border text-center text-sm transition-colors {nodeMode === 'standard' ? 'border-accent font-bold' : 'border-border text-textMuted hover:bg-hover'}"
                            on:click={() => (nodeMode = 'standard')}
                        >
                            Automatic
                        </button>
                        <button
                            type="button"
                            class="no-drag p-3 rounded-full border text-center text-sm transition-colors {nodeMode === 'advanced' ? 'border-accent font-bold' : 'border-border text-textMuted hover:bg-hover'}"
                            on:click={() => (nodeMode = 'advanced')}
                        >
                            Custom
                        </button>
                    </div>
                    <p class="text-sm text-textMuted mt-3">
                        {nodeMode === 'standard' ? 'Recommended. Everything is set up for you.' : 'For experienced users who want to choose their own connection settings.'}
                    </p>
                </div>
            </div>

            <div class="flex gap-3 pt-6">
                <button class="no-drag ef-btn ef-btn-ghost" on:click={prevStep}>Back</button>
                <button class="no-drag ef-btn ef-btn-accent flex-1" on:click={nextStep}>Continue</button>
            </div>
        </div>

    {:else if step === 4}
        <div in:fly={{ y: 12, duration: 200 }}>
            <h2 class="text-2xl font-extrabold mb-1">Create a password</h2>
            <p class="text-sm text-textMuted mb-6">
                This password locks EchoFog on this computer. It never leaves your device, so we can't reset it for you.
            </p>

            <div class="space-y-4">
                <div>
                    <label class="block text-sm font-semibold mb-2" for="master-pwd">Password</label>
                    <div class="relative">
                        {#if showPassword}
                            <input id="master-pwd" type="text" bind:value={appPassword} placeholder="At least 4 characters" class="no-drag ef-input pr-16" />
                        {:else}
                            <input id="master-pwd" type="password" bind:value={appPassword} placeholder="At least 4 characters" class="no-drag ef-input pr-16" />
                        {/if}
                        <button
                            type="button"
                            class="no-drag absolute right-4 top-1/2 -translate-y-1/2 text-sm font-semibold text-accent"
                            on:click={() => (showPassword = !showPassword)}
                        >
                            {showPassword ? 'Hide' : 'Show'}
                        </button>
                    </div>
                </div>

                <div>
                    <label class="block text-sm font-semibold mb-2" for="confirm-pwd">Confirm password</label>
                    {#if showPassword}
                        <input id="confirm-pwd" type="text" bind:value={confirmPassword} placeholder="Type it again" class="no-drag ef-input" />
                    {:else}
                        <input id="confirm-pwd" type="password" bind:value={confirmPassword} placeholder="Type it again" class="no-drag ef-input" />
                    {/if}
                </div>

                <details class="text-sm">
                    <summary class="cursor-pointer text-textMuted">Details (optional extra word)</summary>
                    <div class="mt-3">
                        <label class="block text-sm font-semibold mb-1" for="passphrase-opt">Extra secret word</label>
                        <p class="text-sm text-textMuted mb-2">Adds one more word on top of your recovery words for extra protection. If you set one, you will need it to recover your account, so don't forget it.</p>
                        <input id="passphrase-opt" type="text" bind:value={mnemonicPassphrase} placeholder="Leave empty to skip" class="no-drag ef-input" />
                    </div>
                </details>

                {#if errorMsg}
                    <p class="text-danger text-sm">{errorMsg}</p>
                {/if}
            </div>

            <div class="flex gap-3 pt-6">
                <button class="no-drag ef-btn ef-btn-ghost" on:click={prevStep} disabled={isGenerating}>Back</button>
                <button class="no-drag ef-btn ef-btn-accent flex-1" on:click={handleGenerateKeys} disabled={isGenerating}>
                    {isGenerating ? 'Creating your account…' : 'Create account'}
                </button>
            </div>
        </div>

    {:else if step === 5}
        <div in:fly={{ y: 12, duration: 200 }}>
            <div class="inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-sm font-semibold text-success border border-border mb-4">
                <svg class="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12l5 5L20 7"/></svg>
                Account created
            </div>
            <h2 class="text-2xl font-extrabold mb-1">Save your recovery words</h2>
            <p class="text-sm text-textMuted mb-6">
                These words are the only way to get your account back if you forget your password or switch computers. Write them down on paper and keep them somewhere safe.
            </p>

            <div class="ef-card p-4 mb-4">
                <div class="grid grid-cols-3 sm:grid-cols-4 gap-2 mb-4">
                    {#each mnemonicWords as word, i}
                        <div class="px-2 py-2 rounded-lg text-center border border-border bg-background text-sm select-text">
                            <span class="text-textMuted mr-1">{i + 1}.</span>
                            <span class="font-semibold">{word}</span>
                        </div>
                    {/each}
                </div>

                <button class="no-drag ef-btn ef-btn-ghost w-full" on:click={copySeedPhrase}>
                    {#if copiedMnemonic}
                        <svg class="w-5 h-5 text-success" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12l5 5L20 7"/></svg>
                        Copied
                    {:else}
                        <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V6a2 2 0 012-2h9"/></svg>
                        Copy words
                    {/if}
                </button>
            </div>

            <div class="flex items-start gap-3 p-4 rounded-2xl border border-border text-sm text-textMuted mb-6">
                <svg class="w-5 h-5 shrink-0 text-danger" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 9v4M12 17h.01M10.3 3.9L2.4 17.5A2 2 0 004.1 20.5h15.8a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z"/></svg>
                <span>If you lose these words, nobody can restore your account for you. Never share them with anyone.</span>
            </div>

            <button class="no-drag ef-btn ef-btn-accent w-full h-12" on:click={completeOnboarding}>I've saved them, continue</button>
        </div>
    {/if}
</div>
