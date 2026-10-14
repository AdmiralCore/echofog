/// <reference types="svelte" />
/// <reference types="vite/client" />

// ─── Wails Runtime Type Declarations ──────────────────────────────────────────
// window.go and window.runtime are injected by the Wails2 runtime into WebView2.
// These declarations silence TypeScript errors for Wails-specific globals.

interface WailsGoApp {
    App: {
        // File system
        UploadFile(path: string, transferID: string): Promise<{ id: string; name: string; size: number; root_hash: string; created_at: number }>;
        DownloadFile(id: string, name: string, transferID: string): Promise<string>;
        ListMyFiles(): Promise<Array<{ id: string; name: string; size: number; root_hash: string; created_at: number }>>;
        GenerateShareLink(id: string): Promise<string>;
        TriggerPanicButton(): Promise<void>;
        // Messages / contacts
        SendMessage(pubKey: string, text: string): Promise<void>;
        GetContacts(): Promise<unknown[]>;
        GetChatHistory(targetPeerID: string, limit: number, beforeTimestamp: number): Promise<unknown[]>;
    };
}

interface WailsRuntime {
    EventsOn(eventName: string, callback: (...args: unknown[]) => void): () => void;
    EventsOff(eventName: string): void;
    EventsEmit(eventName: string, ...args: unknown[]): void;
    WindowMinimise(): void;
    WindowMaximise(): void;
    WindowUnmaximise(): void;
    WindowToggleMaximise(): void;
    Quit(): void;
    OpenFileDialog(options?: Record<string, unknown>): Promise<string>;
}

declare global {
    interface Window {
        go: WailsGoApp;
        runtime: WailsRuntime;
        // Legacy webkitAudioContext reference (Safari / old Chromium)
        webkitAudioContext: typeof AudioContext;
    }
}

export {};
