<script>
    import {
        workspaceTabs,
        activeTabId,
        closeTab,
    } from "../../stores/layout.store.js";
    import SettingsApp from "../apps/SettingsApp.svelte";
    import ChatApp from "../../mini-apps/echofog-chat/ChatApp.svelte";
    import ContactsApp from "../../mini-apps/contacts/ContactsApp.svelte";
    import SovereignVPNView from "../views/SovereignVPNView.svelte";
    import WalletView from "../views/WalletView.svelte";
    import { t } from "svelte-i18n";

    function selectTab(id) {
        $activeTabId = id;
    }
</script>

<div
    class="flex-1 bg-background relative min-h-0 transition-all duration-300 w-full h-full no-drag"
>
    {#if $workspaceTabs.length > 0}
        {#each $workspaceTabs as tab (tab.id)}
            <div
                class="w-full h-full {$activeTabId === tab.id
                    ? 'block'
                    : 'hidden'}"
            >
                <!-- Dynamic Component Rendering Based on Tab state -->
                {#if tab.id === "vpn"}
                    <SovereignVPNView />
                {:else if tab.id === "chat"}
                    <ChatApp />
                {:else if tab.id === "settings"}
                    <SettingsApp />
                {:else if tab.id === "contacts"}
                    <ContactsApp />
                {:else if tab.id === "wallet"}
                    <WalletView />
                {:else}
                    <SovereignVPNView />
                {/if}
            </div>
        {/each}
    {:else}
        <div class="w-full h-full flex items-center justify-center text-textMuted text-sm">Select a section</div>
    {/if}
</div>
