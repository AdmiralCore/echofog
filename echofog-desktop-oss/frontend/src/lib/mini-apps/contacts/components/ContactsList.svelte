<script>
    import { onMount, createEventDispatcher } from "svelte";
    import { contacts, contactsLoading, contactsError, loadContacts } from "../stores/contacts.store.js";
    const dispatch = createEventDispatcher();

    onMount(loadContacts);
</script>

<div class="w-full flex flex-col">
    {#each $contacts as contact (contact.peer_id)}
        <button
            class="ef-row w-full px-4 py-3 flex items-center gap-3 text-left transition-colors"
            on:click={() => dispatch("select", contact)}
        >
            <div class="relative w-12 h-12 rounded-full border border-border bg-surface flex items-center justify-center shrink-0">
                {#if contact.avatar_base64}
                    <img
                        src="data:image/png;base64,{contact.avatar_base64}"
                        class="w-full h-full object-cover rounded-full"
                        alt={contact.alias}
                    />
                {:else}
                    <span class="text-text font-bold text-lg">{(contact.alias || contact.peer_id || "?").charAt(0).toUpperCase()}</span>
                {/if}
                <span class="absolute bottom-0 right-0 w-3 h-3 rounded-full border-2 border-background {contact.is_online ? 'bg-success' : 'bg-textMuted'}"></span>
            </div>

            <div class="overflow-hidden flex-1">
                <div class="font-bold text-[15px] truncate">{contact.alias || contact.peer_id}</div>
                <div class="text-sm text-textMuted truncate">{contact.is_online ? "Online" : "Offline"}</div>
            </div>
        </button>
    {/each}

    {#if $contactsError}
        <div class="px-4 py-6 text-sm text-danger">
            {$contactsError}
            <button class="underline ml-1" on:click={loadContacts}>Retry</button>
        </div>
    {:else if $contactsLoading && $contacts.length === 0}
        <div class="text-center py-16 text-textMuted text-sm">Loading</div>
    {:else if $contacts.length === 0}
        <div class="text-center py-16 text-textMuted text-sm">No one here yet</div>
    {/if}
</div>
