<script>
  import { onMount } from "svelte";
  import "./app.css";
  import { isLoading } from "svelte-i18n";
  import "./lib/i18n.js";
  import {
    authState,
    bootStatus,
    checkSystemState,
    bootCoreNode,
    pickProfile,
    profilesList,
    currentProfile,
    removeIdentity,
  } from "./lib/stores/auth.store";

  import AppShell from "./lib/components/layout/AppShell.svelte";
  import ImportWallet from "./lib/components/auth/ImportWallet.svelte";
  import OnboardingWizard from "./lib/components/auth/OnboardingWizard.svelte";
  import TerminalBoot from "./lib/components/auth/TerminalBoot.svelte";
  import SettingsApp from "./lib/components/apps/SettingsApp.svelte";
  import ChatApp from "./lib/mini-apps/echofog-chat/ChatApp.svelte";
  import BootLoader from "./lib/components/common/BootLoader.svelte";
  import ErrorBoundary from "./lib/components/common/ErrorBoundary.svelte";
  import { currentTheme, toggleTheme } from "./lib/stores/theme.store.js";

  let passwordInput = "";
  let loginError = "";
  let newProfileName = "";
  let showPassword = false;

  // Deletion Modal State
  let showDeleteModal = false;
  let profileToDelete = "";
  let profileActionsOverride = null; // Stores profile name whose actions are forced visible by RMB

  function openDeleteModal(profile) {
    profileToDelete = profile;
    showDeleteModal = true;
    profileActionsOverride = null; // Reset override after action
  }

  function handleProfileContextMenu(profile) {
    profileActionsOverride =
      profile === profileActionsOverride ? null : profile;
  }

  async function confirmDelete() {
    if (!profileToDelete) return;
    try {
      await removeIdentity(profileToDelete);
      showDeleteModal = false;
      profileToDelete = "";
    } catch (err) {
      console.error("Delete failed:", err);
    }
  }

  $: if ($authState === "profiles") {
    passwordInput = "";
  }

  // Detached Tab State
  let isDetached = false;
  let detachedTabId = "";

  onMount(async () => {
    const staticLoader = document.getElementById("initial-loader");
    if (staticLoader) {
      staticLoader.remove();
    }

    const urlParams = new URLSearchParams(window.location.search);
    isDetached = urlParams.get("detached") === "true";
    if (isDetached) {
      detachedTabId = urlParams.get("tabId") || "";
      return; // Skip normal setup
    }

    try {
      await checkSystemState();
    } catch (err) {
      console.error("[App] Startup error:", err);
      authState.set("create");
    }
  });

  function goToCreate() {
    authState.set("create");
  }
  function goToImport() {
    authState.set("import");
  }
  function goToMenu() {
    authState.set("onboarding");
  }
  function goToProfiles() {
    newProfileName = "";
    checkSystemState();
    authState.set("profiles");
    passwordInput = "";
  }

  async function handleLogin() {
    if (!passwordInput) return;
    loginError = "";
    try {
      await window.go.main.App.Login(passwordInput);
      await bootCoreNode();
      authState.set("app");
    } catch (err) {
      loginError = String(err);
      console.error(err);
    }
  }

  async function handleAuthComplete(event) {
    try {
      await bootCoreNode();
    } catch (err) {
      console.error("Failed to start node:", err);
    }
    authState.set("app");
  }

  async function finalizeSystemBoot() {
    try {
      await bootCoreNode();
    } catch (err) {
      console.error("Failed to boot node after terminal sequence:", err);
    }
  }
</script>

<ErrorBoundary>
<main class="w-screen h-screen overflow-hidden bg-background text-text font-sans select-none">
  {#if isDetached}
    <div class="w-full h-full bg-background text-text">
      {#if detachedTabId === "settings"}
        <SettingsApp />
      {:else if detachedTabId === "chat"}
        <div class="flex w-full h-full"><ChatApp /></div>
      {:else}
        <div class="flex items-center justify-center w-full h-full text-textMuted text-sm">Loading…</div>
      {/if}
    </div>
  {:else if $authState === "booting"}
    <BootLoader statusText={$bootStatus} onBypass={() => authState.set('create')} />
  {:else if $authState === "system_init"}
    <TerminalBoot on:complete={finalizeSystemBoot} />
  {:else if $authState === "app"}
    <AppShell />
  {:else}
    <button class="no-drag ef-icon-btn absolute top-4 right-4 z-50" on:click={toggleTheme} title="Switch theme" aria-label="Switch theme">
      {#if $currentTheme === 'super-light'}
        <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20.35 15.35A9 9 0 018.65 3.65 9 9 0 1020.35 15.35z"/></svg>
      {:else}
        <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>
      {/if}
    </button>

    <div class="relative w-full h-full flex items-center justify-center p-4">
      {#if $authState === "profiles"}
        <div class="w-full max-w-sm">
          <div class="w-12 h-12 rounded-full bg-accent mx-auto mb-6 flex items-center justify-center text-white font-bold text-xl">E</div>
          <h1 class="text-2xl font-extrabold text-center mb-6">Choose account</h1>

          {#if $profilesList.length > 0}
            <div class="mb-6 max-h-[40vh] overflow-y-auto">
              {#each $profilesList as profile}
                <div class="group flex items-center rounded-xl ef-row" on:contextmenu|preventDefault={() => handleProfileContextMenu(profile)}>
                  <button class="no-drag flex-1 flex items-center gap-3 px-3 py-3 text-left" on:click={() => pickProfile(profile)}>
                    <span class="w-10 h-10 rounded-full bg-elevated border border-border flex items-center justify-center font-bold uppercase">{profile.slice(0, 1)}</span>
                    <span class="font-semibold">{profile}</span>
                  </button>
                  <button
                    class="no-drag ef-icon-btn mr-2 transition-opacity {profileActionsOverride === profile ? 'opacity-100' : 'opacity-0 group-hover:opacity-100'}"
                    on:click|stopPropagation={() => openDeleteModal(profile)}
                    title="Delete account" aria-label="Delete account">
                    <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14"/></svg>
                  </button>
                </div>
              {/each}
            </div>
          {:else}
            <p class="text-center text-textMuted text-sm mb-6">No accounts on this device yet.</p>
          {/if}

          <div class="flex gap-2">
            <input type="text" class="no-drag ef-input" bind:value={newProfileName} placeholder="New account name"
              on:keydown={(e) => { if (e.key === "Enter") { pickProfile(newProfileName.trim() || "main"); newProfileName = ""; } }} />
            <button class="no-drag ef-btn shrink-0" on:click={() => { pickProfile(newProfileName.trim() || "main"); newProfileName = ""; }}>Create</button>
          </div>
        </div>
      {:else if $authState === "login"}
        <button class="no-drag absolute top-4 left-4 ef-btn ef-btn-ghost" on:click={goToProfiles}>← Back</button>
        <div class="w-full max-w-sm">
          <div class="w-12 h-12 rounded-full bg-elevated border border-border mx-auto mb-4 flex items-center justify-center font-bold text-lg uppercase">{($currentProfile || "?").slice(0, 1)}</div>
          <h1 class="text-2xl font-extrabold text-center mb-1">{$currentProfile}</h1>
          <p class="text-sm text-textMuted text-center mb-6">Enter your password to continue</p>

          <div class="relative">
            <input type={showPassword ? "text" : "password"} placeholder="Password" class="no-drag ef-input pr-11"
              value={passwordInput}
              on:input={(e) => (passwordInput = e.currentTarget.value)}
              on:keydown={(e) => e.key === "Enter" && handleLogin()} />
            <button class="no-drag absolute inset-y-0 right-2 ef-icon-btn my-auto" on:click={() => (showPassword = !showPassword)} tabindex="-1" aria-label="Show password">
              <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
                {#if showPassword}<path d="M3 3l18 18M10.6 6.1A10 10 0 0122 12a10 10 0 01-3.2 4.4M6.2 6.2A10 10 0 002 12c1.3 4.3 5.3 7.5 10 7.5 1 0 1.9-.1 2.9-.4"/>{:else}<path d="M2 12s3.5-7.5 10-7.5S22 12 22 12s-3.5 7.5-10 7.5S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>{/if}
              </svg>
            </button>
          </div>

          {#if loginError}
            <p class="text-danger text-sm mt-3 text-center">Wrong password</p>
          {/if}

          <button class="no-drag ef-btn w-full mt-5 h-12" on:click={handleLogin}>Log in</button>
        </div>
      {:else if $authState === "onboarding" || $authState === "create"}
        <button class="no-drag absolute top-4 left-4 ef-btn ef-btn-ghost" on:click={goToProfiles}>← Back</button>
        <OnboardingWizard on:complete={handleAuthComplete} on:cancel={goToProfiles} />
      {:else if $authState === "import"}
        <ImportWallet on:complete={handleAuthComplete} on:cancel={goToMenu} />
      {/if}
    </div>
  {/if}

  {#if showDeleteModal}
    <div class="fixed inset-0 z-[100] flex items-center justify-center bg-black/60 p-4">
      <div class="ef-card w-full max-w-sm p-6 bg-background">
        <h2 class="text-xl font-extrabold mb-2">Delete account?</h2>
        <p class="text-sm text-textMuted mb-6">“{profileToDelete}” and its data will be removed from this device. This cannot be undone.</p>
        <div class="flex flex-col gap-2">
          <button class="no-drag ef-btn ef-btn-danger" on:click={confirmDelete}>Delete</button>
          <button class="no-drag ef-btn ef-btn-ghost" on:click={() => (showDeleteModal = false)}>Cancel</button>
        </div>
      </div>
    </div>
  {/if}
</main>
</ErrorBoundary>
