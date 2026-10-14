<script>
    import ChatCanvas from "./components/ChatCanvas.svelte";
    import CommandCenterPanel from "./components/CommandCenterPanel.svelte";
    import NavigatorPanel from "./components/NavigatorPanel.svelte";
</script>

<div class="w-full h-full flex overflow-hidden bg-background">
    <div class="w-[30%] min-w-[280px] max-w-[380px] flex flex-col border-r border-border shrink-0">
        <NavigatorPanel />
    </div>
    <div class="flex-1 flex overflow-hidden relative min-w-0 text-text">
        <ChatCanvas />
        <CommandCenterPanel />
    </div>
</div>
