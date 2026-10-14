<script>
    import QRScanner from "./QRScanner.svelte";
    import { addContact } from "../stores/contacts.store.js";

    let mode = "manual"; // 'manual', 'scanner'
    let alias = "";
    let peerId = "";
    let pubKey = "";
    let saving = false;
    let errorMsg = "";
    let successMsg = "";

    const HEX = /^[0-9a-fA-F]+$/;
    let avatarBase64 = "";

    function handleFileChange(e) {
        const file = e.target.files[0];
        if (file) {
            const reader = new FileReader();
            reader.onload = (event) => {
                const result = event.target.result;
                if (typeof result === "string") {
                    avatarBase64 = result.split(",")[1];
                }
            };
            reader.readAsDataURL(file);
        }
    }

    async function handleSave() {
        errorMsg = "";
        successMsg = "";
        const id = peerId.trim();
        const key = pubKey.trim();
        if (!HEX.test(id)) {
            errorMsg = "Contact ID must be a hex string.";
            return;
        }
        if (!HEX.test(key)) {
            errorMsg = "Public key must be a hex string.";
            return;
        }
        saving = true;
        try {
            await addContact({
                peer_id: id,
                pub_key: key,
                alias: alias.trim() || id.slice(0, 8),
                avatar_base64: avatarBase64,
                is_online: false,
                last_seen: 0,
            });
            successMsg = "Person added.";
            alias = "";
            peerId = "";
            pubKey = "";
            avatarBase64 = "";
        } catch (err) {
            errorMsg = (err && (err.message || String(err))) || "Could not add this person.";
        } finally {
            saving = false;
        }
    }

    function handleScanSuccess(e) {
        const decodedText = e.detail;
        try {
            // Assume format: "echofog://contact?peerId=...&alias=..." or JSON payload
            if (decodedText.startsWith("echofog://")) {
                const url = new URL(decodedText);
                peerId = url.searchParams.get("peerId") || "";
                pubKey = url.searchParams.get("pubKey") || "";
                alias = url.searchParams.get("alias") || "Scanned Peer";
            } else {
                const data = JSON.parse(decodedText);
                peerId = data.peer_id || data.peerId || "";
                pubKey = data.pub_key || data.pubKey || "";
                alias = data.alias || "Scanned Peer";
            }
            mode = "manual"; // switch back to manual entry to confirm
        } catch (err) {
            // Fallback, just paste the text into peerId
            peerId = decodedText;
            mode = "manual";
        }
    }
</script>

<div class="ef-card w-full max-w-lg p-6 no-drag">
    <div class="flex gap-2 mb-6">
        <button
            class="ef-btn flex-1 {mode === 'manual' ? 'ef-btn-accent' : 'ef-btn-ghost'}"
            on:click={() => (mode = "manual")}
        >
            Type it in
        </button>
        <button
            class="ef-btn flex-1 {mode === 'scanner' ? 'ef-btn-accent' : 'ef-btn-ghost'}"
            on:click={() => (mode = "scanner")}
        >
            Scan a code
        </button>
    </div>

    {#if mode === "manual"}
        <div class="space-y-5">
            <div>
                <label for="alias-input" class="block text-sm font-bold mb-2">Name</label>
                <input
                    id="alias-input"
                    type="text"
                    bind:value={alias}
                    placeholder="What do you call them?"
                    class="ef-input"
                />
            </div>

            <div>
                <label for="peer-id-input" class="block text-sm font-bold mb-1">Contact ID</label>
                <p class="text-xs text-textMuted mb-2">Ask them to share it from their "My code" screen.</p>
                <input
                    id="peer-id-input"
                    type="text"
                    bind:value={peerId}
                    placeholder="12D3KooW..."
                    class="ef-input font-mono text-sm"
                />
            </div>

            <div>
                <label for="pub-key-input" class="block text-sm font-bold mb-2">Public key</label>
                <input
                    id="pub-key-input"
                    type="text"
                    bind:value={pubKey}
                    placeholder="Shared together with the contact ID"
                    class="ef-input font-mono text-sm"
                />
            </div>

            <div>
                <label for="avatar-upload" class="block text-sm font-bold mb-2">Photo (optional)</label>
                <div class="flex items-center gap-4">
                    <div class="w-16 h-16 rounded-full border border-dashed border-border flex items-center justify-center overflow-hidden bg-surface text-textMuted">
                        {#if avatarBase64}
                            <img
                                src="data:image/png;base64,{avatarBase64}"
                                class="w-full h-full object-cover"
                                alt="Preview"
                            />
                        {:else}
                            <svg class="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="3"/><circle cx="9" cy="10" r="2"/><path d="M21 16l-5-5-9 9"/></svg>
                        {/if}
                    </div>
                    <label class="ef-btn ef-btn-ghost cursor-pointer">
                        Choose photo
                        <input
                            id="avatar-upload"
                            type="file"
                            accept="image/*"
                            class="hidden"
                            on:change={handleFileChange}
                        />
                    </label>
                    {#if avatarBase64}
                        <button
                            class="text-sm text-danger font-bold hover:underline"
                            on:click={() => (avatarBase64 = "")}>Remove</button
                        >
                    {/if}
                </div>
            </div>

            {#if errorMsg}<div class="text-sm text-danger">{errorMsg}</div>{/if}
            {#if successMsg}<div class="text-sm text-success">{successMsg}</div>{/if}

            <button class="ef-btn ef-btn-accent w-full" on:click={handleSave} disabled={!peerId || !pubKey || saving}>
                {saving ? "Adding..." : "Add person"}
            </button>
        </div>
    {:else if mode === "scanner"}
        <div class="w-full h-[400px] bg-black rounded-2xl overflow-hidden relative border border-border">
            <QRScanner on:scan={handleScanSuccess} />
        </div>
    {/if}
</div>
