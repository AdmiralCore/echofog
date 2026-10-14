<script>
    import { onMount } from "svelte";
    import QRCode from "qrcode";
    import { GetMyID, GetMyPubKey } from "@wailsjs/go/main/App";
    import { currentProfile } from "../../../stores/auth.store.js";

    let qrCodeDataUrl = "";

    let myPeerId = "";
    let myPubKey = "";
    let loadError = "";


    onMount(async () => {
        try {
            myPeerId = await GetMyID();
            myPubKey = await GetMyPubKey();
            if (!myPeerId) {
                loadError = "Your node is not running yet.";
                return;
            }
            qrCodeDataUrl = await QRCode.toDataURL(JSON.stringify({ alias: $currentProfile || "", peer_id: myPeerId, pub_key: myPubKey }), {
                errorCorrectionLevel: "H",
                margin: 2,
                color: {
                    dark: "#000000ff",
                    light: "#ffffff00",
                },
            });
        } catch (err) {
            loadError = (err && (err.message || String(err))) || "Could not build your code.";
        }
    });

    let copymsg = "Copy";
    function copyPeerId() {
        navigator.clipboard.writeText(myPeerId);
        copymsg = "Copied";
        setTimeout(() => (copymsg = "Copy"), 2000);
    }
</script>

<div class="ef-card flex flex-col items-center p-8 no-drag max-w-md w-full">
    <div class="text-sm text-textMuted text-center">Your code</div>
    <div class="text-xl font-extrabold mb-6 text-center">
        {$currentProfile || "Me"}
    </div>

    <div class="p-4 bg-white rounded-2xl mb-6 shrink-0">
        {#if qrCodeDataUrl}
            <img
                src={qrCodeDataUrl}
                alt="My QR Code"
                class="w-60 h-60 object-contain"
                style="image-rendering: pixelated;"
            />
        {:else if loadError}
            <div class="w-60 h-60 flex items-center justify-center text-sm text-danger text-center">{loadError}</div>
        {:else}
            <div class="w-60 h-60 flex items-center justify-center">
                <div class="w-10 h-10 border-4 border-accent border-t-transparent rounded-full animate-spin"></div>
            </div>
        {/if}
    </div>

    <p class="text-sm text-textMuted text-center mb-4">Let a friend scan this code to add you.</p>

    <details class="w-full">
        <summary class="text-sm text-textMuted cursor-pointer">Details</summary>
        <label for="raw-peer-id" class="block text-sm font-bold mt-3 mb-2">Your contact ID</label>
        <div class="flex gap-2">
            <input
                id="raw-peer-id"
                type="text"
                readonly
                value={myPeerId}
                class="ef-input font-mono text-xs cursor-pointer"
                on:click={copyPeerId}
            />
            <button class="ef-btn ef-btn-ghost shrink-0" on:click={copyPeerId}>{copymsg}</button>
        </div>
        <label for="raw-pub-key" class="block text-sm font-bold mt-3 mb-2">Your public key</label>
        <input id="raw-pub-key" type="text" readonly value={myPubKey} class="ef-input font-mono text-xs" />
    </details>
</div>
