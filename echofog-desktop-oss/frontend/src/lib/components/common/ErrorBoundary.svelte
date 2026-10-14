<script>
    export let error = null;
    export let onReset = () => window.location.reload();
</script>

{#if error}
    <div class="fixed inset-0 z-50 flex items-center justify-center p-6 bg-background text-text select-none">
        <div class="ef-card max-w-md w-full p-6 bg-background">
            <div class="flex items-center gap-3 text-danger mb-3">
                <svg class="w-6 h-6 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 9v4M12 17h.01M10.3 3.9L2.4 17.5A2 2 0 004.1 20.5h15.8a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z"/></svg>
                <h3 class="text-xl font-extrabold text-text">Something went wrong</h3>
            </div>
            <p class="text-sm text-textMuted mb-4">EchoFog ran into a problem. Trying again usually fixes it.</p>
            <details class="mb-5">
                <summary class="text-sm text-textMuted cursor-pointer">Details</summary>
                <pre class="text-xs font-mono p-3 mt-2 rounded-lg overflow-x-auto bg-surface border border-border text-textMuted select-text">{error.message || String(error)}</pre>
            </details>
            <button class="no-drag ef-btn w-full" on:click={onReset}>Try again</button>
        </div>
    </div>
{:else}
    <slot />
{/if}
