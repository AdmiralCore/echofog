<script>
    import ContactsList from "./components/ContactsList.svelte";
    import AddContactFlow from "./components/AddContactFlow.svelte";
    import MyQR from "./components/MyQR.svelte";
    import { removeContact } from "./stores/contacts.store.js";
    import { openApp } from "../../stores/layout.store.js";

    let activeView = "list"; // 'list', 'add', 'my_qr'
    let selectedContact = null;

    let deleteError = "";

    async function handleDelete() {
        deleteError = "";
        try {
            await removeContact(selectedContact.peer_id);
            selectedContact = null;
            activeView = "list";
        } catch (err) {
            deleteError = (err && (err.message || String(err))) || "Could not delete this person.";
        }
    }

    function handleViewChange(view) {
        activeView = view;
        selectedContact = null;
    }
</script>

<div class="w-full h-full flex overflow-hidden bg-background text-text no-drag">
    <!-- Left Column: Contact List -->
    <div class="w-80 h-full border-r border-border flex flex-col shrink-0">
        <div class="px-4 pt-4 pb-2 flex items-center justify-between shrink-0">
            <h1 class="text-xl font-extrabold">People</h1>
        </div>

        <div class="px-4 pb-3 flex gap-2 shrink-0">
            <button
                class="ef-btn flex-1 {activeView === 'add' ? 'ef-btn-accent' : 'ef-btn-ghost'}"
                on:click={() => handleViewChange("add")}
            >
                Add person
            </button>
            <button
                class="ef-btn flex-1 {activeView === 'my_qr' ? 'ef-btn-accent' : 'ef-btn-ghost'}"
                on:click={() => handleViewChange("my_qr")}
            >
                My code
            </button>
        </div>

        <div class="flex-1 overflow-y-auto">
            <ContactsList
                on:select={(e) => {
                    selectedContact = e.detail;
                    activeView = "details";
                }}
            />
        </div>
    </div>

    <!-- Right Column -->
    <div class="flex-1 h-full flex items-center justify-center p-8 overflow-y-auto">
        {#if activeView === "add"}
            <AddContactFlow />
        {:else if activeView === "my_qr"}
            <MyQR />
        {:else if activeView === "details" && selectedContact}
            <div class="ef-card text-center w-full max-w-md p-8">
                <div class="w-28 h-28 rounded-full border border-border mx-auto mb-5 bg-elevated overflow-hidden">
                    {#if selectedContact.avatar_base64}
                        <img
                            src="data:image/png;base64,{selectedContact.avatar_base64}"
                            alt="Avatar"
                            class="w-full h-full object-cover"
                        />
                    {:else}
                        <div class="w-full h-full flex items-center justify-center text-4xl text-textMuted font-bold">
                            {selectedContact.alias?.[0]?.toUpperCase() || "?"}
                        </div>
                    {/if}
                </div>
                <h3 class="text-xl font-extrabold mb-1">{selectedContact.alias}</h3>
                <div class="inline-flex items-center gap-2 text-sm text-textMuted mb-1">
                    <span class="w-2.5 h-2.5 rounded-full {selectedContact.is_online ? 'bg-success' : 'bg-textMuted'}"></span>
                    {selectedContact.is_online ? "Online" : "Offline"}
                </div>
                {#if !selectedContact.is_online && selectedContact.last_seen}
                    <div class="text-xs text-textMuted mb-4">
                        Last seen {new Date(selectedContact.last_seen * 1000).toLocaleString()}
                    </div>
                {/if}

                <details class="text-left mt-4">
                    <summary class="text-sm text-textMuted cursor-pointer">Details</summary>
                    <p class="mt-2 text-xs text-textMuted font-mono bg-surface p-3 rounded-lg border border-border break-all cursor-text select-all">
                        {selectedContact.peer_id}
                    </p>
                </details>

                <div class="mt-6 flex gap-3 justify-center">
                    <button class="ef-btn ef-btn-accent" on:click={() => openApp("chat", "Chats")}>Message</button>
                    <button class="ef-btn ef-btn-ghost" on:click={() => openApp("wallet", "Wallet")}>Wallet</button>
                    <button class="ef-btn ef-btn-ghost text-danger" on:click={handleDelete}>Delete</button>
                </div>
                {#if deleteError}<div class="text-sm text-danger mt-3">{deleteError}</div>{/if}
            </div>
        {:else}
            <div class="text-center text-textMuted pointer-events-none select-none">
                <svg class="w-14 h-14 mx-auto mb-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c0-3.6 2.9-6 6.5-6s6.5 2.4 6.5 6"/><path d="M16 4.5a3.5 3.5 0 010 7M18 14.5c2 .7 3.5 2.5 3.5 5.5"/></svg>
                <div class="text-xl font-extrabold text-text mb-1">Your people</div>
                <div class="text-[15px]">Pick someone from the list or add a new person.</div>
            </div>
        {/if}
    </div>
</div>
