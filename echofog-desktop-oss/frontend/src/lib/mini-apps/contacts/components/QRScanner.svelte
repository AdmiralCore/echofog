<script>
    import { onMount, onDestroy, createEventDispatcher } from "svelte";
    import { Html5Qrcode } from "html5-qrcode";

    const dispatch = createEventDispatcher();
    let scannerElement;
    let html5QrCode;
    let scanning = false;
    let scanError = null;

    onMount(() => {
        html5QrCode = new Html5Qrcode("reader");
        startScanner();
    });

    onDestroy(() => {
        if (scanning) {
            html5QrCode
                .stop()
                .then(() => {
                    html5QrCode.clear();
                })
                .catch((err) => {
                    console.error("Error stopping scanner", err);
                });
        }
    });

    async function startScanner() {
        try {
            scanning = true;
            await html5QrCode.start(
                { facingMode: "environment" },
                {
                    fps: 10,
                    qrbox: { width: 250, height: 250 },
                    aspectRatio: 1.0,
                },
                (decodedText, decodedResult) => {
                    if (scanning) {
                        html5QrCode.stop().then(() => {
                            scanning = false;
                            dispatch("scan", decodedText);
                        });
                    }
                },
                (errorMessage) => {
                    // Ignored visually
                },
            );
        } catch (err) {
            console.error("Scanner init error:", err);
            scanError = err.message || "Failed to access camera.";
            scanning = false;
        }
    }
</script>

<div class="w-full h-full relative flex flex-col items-center justify-center bg-black">
    <div id="reader" bind:this={scannerElement} class="w-full h-full"></div>

    <div class="absolute inset-0 pointer-events-none flex items-center justify-center z-10">
        <div class="w-[250px] h-[250px] relative">
            <div class="absolute top-0 left-0 w-8 h-8 border-t-4 border-l-4 border-white rounded-tl-xl"></div>
            <div class="absolute top-0 right-0 w-8 h-8 border-t-4 border-r-4 border-white rounded-tr-xl"></div>
            <div class="absolute bottom-0 left-0 w-8 h-8 border-b-4 border-l-4 border-white rounded-bl-xl"></div>
            <div class="absolute bottom-0 right-0 w-8 h-8 border-b-4 border-r-4 border-white rounded-br-xl"></div>
            <div class="absolute top-0 left-0 right-0 h-0.5 bg-white opacity-50 scan-line"></div>
        </div>
    </div>

    {#if scanError}
        <div class="absolute inset-x-0 bottom-4 text-center text-danger text-sm font-bold px-4 bg-black/80 py-2">
            Couldn't use the camera: {scanError}
        </div>
    {:else}
        <div class="absolute bottom-6 text-white text-sm font-semibold bg-black/70 px-4 py-2 rounded-full">
            Point the camera at a code
        </div>
    {/if}
</div>

<style>
    .scan-line {
        animation: scan 2s infinite linear;
    }
    @keyframes scan {
        0% {
            transform: translateY(0);
            opacity: 0;
        }
        10% {
            opacity: 1;
        }
        90% {
            opacity: 1;
        }
        100% {
            transform: translateY(250px);
            opacity: 0;
        }
    }

    :global(#reader__dashboard_section_csr),
    :global(#reader__dashboard_section_swaplink) {
        display: none !important;
    }
    :global(#reader__scan_region) {
        display: flex;
        justify-content: center;
        align-items: center;
    }
    :global(#reader video) {
        object-fit: cover !important;
        width: 100% !important;
        height: 100% !important;
    }
</style>
