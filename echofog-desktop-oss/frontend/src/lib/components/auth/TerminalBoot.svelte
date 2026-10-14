<script>
    import { createEventDispatcher, onMount, onDestroy } from "svelte";
    import { fade } from "svelte/transition";

    const dispatch = createEventDispatcher();

    const stages = [
        { at: 0, text: "Getting things ready…" },
        { at: 800, text: "Setting up your account…" },
        { at: 1600, text: "Connecting to the network…" },
        { at: 2300, text: "Almost there…" },
    ];

    let progress = 0;
    let status = stages[0].text;
    let timers = [];

    onMount(() => {
        // Let the bar start from 0, then animate to 100% via CSS transition
        timers.push(setTimeout(() => (progress = 100), 50));

        stages.forEach((s) => {
            timers.push(setTimeout(() => (status = s.text), s.at));
        });

        timers.push(
            setTimeout(() => {
                dispatch("complete");
            }, 2800),
        );
    });

    onDestroy(() => {
        timers.forEach(clearTimeout);
    });
</script>

<div
    class="fixed inset-0 z-[100] w-full h-full bg-background text-text flex flex-col items-center justify-center p-6 select-none"
    transition:fade={{ duration: 300 }}
>
    <div class="w-12 h-12 rounded-full bg-accent mb-6 flex items-center justify-center text-white font-bold text-xl">E</div>
    <h1 class="text-2xl font-extrabold mb-6">Starting EchoFog…</h1>

    <div class="w-64 h-1 rounded-full bg-border overflow-hidden mb-4">
        <div
            class="h-full rounded-full bg-accent"
            style="width: {progress}%; transition: width 2.6s ease-out;"
        ></div>
    </div>

    <p class="text-sm text-textMuted">{status}</p>
</div>
