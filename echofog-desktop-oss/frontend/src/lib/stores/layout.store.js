import { writable, get } from 'svelte/store';

// Open sections in the workspace (array of { id, title, type }) and the focused one.
export const workspaceTabs = writable([{ id: "chat", title: "Chats", type: "chat" }]);
export const activeTabId = writable("chat");

// Sections of the launch build: chats (with calls), contacts, proxy, wallet, settings.
export const AVAILABLE_APPS = [
    { id: "chat", label: "Chats", icon: "M21 12a8 8 0 01-11.6 7.1L3 21l1.9-5.4A8 8 0 1121 12z", title: "Chats" },
    { id: "contacts", label: "Contacts", icon: "M16 21v-2a4 4 0 00-4-4H6a4 4 0 00-4 4v2M9 11a4 4 0 100-8 4 4 0 000 8zM22 21v-2a4 4 0 00-3-3.9M16 3.1a4 4 0 010 7.8", title: "Contacts" },
    { id: "vpn", label: "Proxy", icon: "M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6l8-3z", title: "Private connection" },
    { id: "wallet", label: "Wallet", icon: "M3 7a2 2 0 012-2h14v4M3 7v10a2 2 0 002 2h16V9H5a2 2 0 01-2-2zM16 14h2", title: "Wallet" },
    { id: "logs", label: "Logs", icon: "M4 4h16v16H4zM8 9h8M8 13h8M8 17h5", title: "Logs" },
    { id: "settings", label: "Settings", icon: "M12 15a3 3 0 100-6 3 3 0 000 6zM19.4 15a1.7 1.7 0 00.3 1.8l.1.1a2 2 0 11-2.8 2.8l-.1-.1a1.7 1.7 0 00-1.8-.3 1.7 1.7 0 00-1 1.5V21a2 2 0 11-4 0v-.1a1.7 1.7 0 00-1.1-1.5 1.7 1.7 0 00-1.8.3l-.1.1a2 2 0 11-2.8-2.8l.1-.1a1.7 1.7 0 00.3-1.8 1.7 1.7 0 00-1.5-1H3a2 2 0 110-4h.1a1.7 1.7 0 001.5-1.1 1.7 1.7 0 00-.3-1.8l-.1-.1a2 2 0 112.8-2.8l.1.1a1.7 1.7 0 001.8.3H9a1.7 1.7 0 001-1.5V3a2 2 0 114 0v.1a1.7 1.7 0 001 1.5 1.7 1.7 0 001.8-.3l.1-.1a2 2 0 112.8 2.8l-.1.1a1.7 1.7 0 00-.3 1.8V9a1.7 1.7 0 001.5 1H21a2 2 0 110 4h-.1a1.7 1.7 0 00-1.5 1z", title: "Settings" }
];

export function openApp(appType, title) {
    const tabs = get(workspaceTabs);
    const existingIndex = tabs.findIndex(tab => tab.id === appType);

    if (existingIndex !== -1) {
        // Tab exists, just focus it
        activeTabId.set(appType);
    } else {
        // Create new tab
        const newTab = { id: appType, title: title, type: appType };
        workspaceTabs.update(t => [...t, newTab]);
        activeTabId.set(appType);
    }
}

export function closeTab(tabId) {
    const tabs = get(workspaceTabs);
    const index = tabs.findIndex(tab => tab.id === tabId);

    if (index === -1) return; // Tab not found

    // Filter out the closed tab
    const newTabs = tabs.filter(tab => tab.id !== tabId);
    workspaceTabs.set(newTabs);

    // If we closed the active tab, we need to focus another one
    const currentActiveTab = get(activeTabId);
    if (currentActiveTab === tabId) {
        if (newTabs.length > 0) {
            // Select the one to the left if available, or right
            const newIndex = Math.min(index, newTabs.length - 1);
            activeTabId.set(newTabs[newIndex].id);
        } else {
            // No tabs left
            activeTabId.set(null);
        }
    }
}
