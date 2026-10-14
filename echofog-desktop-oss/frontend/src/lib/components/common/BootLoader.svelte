<script>
    import { onMount, onDestroy } from "svelte";

    export let statusText = "Starting EchoFog…";
    export let onBypass = () => {};

    let showFallback = false;
    let timer;

    onMount(() => {
        timer = setTimeout(() => {
            showFallback = true;
        }, 2500);
    });

    onDestroy(() => {
        if (timer) clearTimeout(timer);
    });
</script>

<div class="fixed inset-0 z-50 flex flex-col items-center justify-center p-6 select-none bg-background text-text">
    <div class="flex flex-col items-center max-w-sm text-center">
        <div class="w-12 h-12 rounded-full bg-accent mb-5 flex items-center justify-center text-white font-bold text-xl">E</div>

        <h1 class="text-2xl font-extrabold mb-4">EchoFog</h1>

        <div class="flex items-center gap-3 text-sm text-textMuted mb-4">
            <div class="w-4 h-4 rounded-full border-2 border-current border-t-transparent animate-spin"></div>
            <span>{statusText}</span>
        </div>

        {#if showFallback}
            <div class="ef-card mt-4 p-4 text-left w-full">
                <div class="font-bold text-[15px] mb-1">Taking longer than usual</div>
                <div class="text-sm text-textMuted mb-4">You can keep waiting or continue to account setup.</div>
                <button class="no-drag ef-btn w-full" on:click={onBypass}>Continue</button>
            </div>
        {/if}
    </div>
</div>
