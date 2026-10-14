import { writable, get } from 'svelte/store';
import { BootService } from '../services/boot.service';
import { initNetworkListeners } from './network.store';
import { initChatListeners } from './chat.store';

// Auth State machine: 'booting' | 'profiles' | 'login' | 'onboarding' | 'create' | 'import' | 'app'
export const authState = writable('booting');
export const bootStatus = writable('Starting EchoFog runtime...');
export const profilesList = writable([]);
export const currentProfile = writable("");

export const logout = async () => {
    try {
        if (window.go?.main?.App?.Logout) {
            await window.go.main.App.Logout();
        }
    } catch (e) {
        console.error("[AuthStore] Failed to logout node:", e);
    }
    currentProfile.set("");
    await checkSystemState();
    authState.set('profiles');
};

export const checkSystemState = async () => {
    bootStatus.set('Connecting to node vault...');
    try {
        const isReady = await BootService.waitForBridge(2500);
        if (!isReady || !window.go?.main?.App?.GetProfiles) {
            console.warn("[AuthStore] Bridge did not report GetProfiles in time; proceeding to onboarding fallback.");
            currentProfile.set("default");
            authState.set('create');
            return;
        }

        bootStatus.set('Reading existing identities...');
        const profiles = (await window.go.main.App.GetProfiles()) || [];
        profilesList.set(profiles);

        // First run check: If no profiles exist, launch the Onboarding Wizard immediately
        if (profiles.length === 0) {
            if (window.go.main.App.SelectProfile) {
                await window.go.main.App.SelectProfile("default");
            }
            currentProfile.set("default");
            authState.set('create');
            return;
        }

        // Profiles exist: show identity selector
        authState.set('profiles');
    } catch (e) {
        console.error("[AuthStore] Boot check error:", e);
        // Resilient fallback: Never get stuck on black screen
        currentProfile.set("default");
        authState.set('create');
    }
};

export const pickProfile = async (profileName) => {
    if (!profileName || !profileName.trim()) return;

    try {
        const safeName = profileName.trim();
        if (window.go?.main?.App?.SelectProfile) {
            await window.go.main.App.SelectProfile(safeName);
        }
        currentProfile.set(safeName);

        let isReg = false;
        if (window.go?.main?.App?.IsRegistered) {
            isReg = await window.go.main.App.IsRegistered();
        }

        authState.set(isReg ? 'login' : 'create');
    } catch (e) {
        console.error("[AuthStore] Failed to select profile:", e);
        // Fallback to login or create depending on error
        authState.set('create');
    }
};

export const removeIdentity = async (profileName) => {
    if (!profileName) return;

    try {
        if (window.go?.main?.App?.DeleteProfile) {
            await window.go.main.App.DeleteProfile(profileName);
        }
        const profiles = (window.go?.main?.App?.GetProfiles && await window.go.main.App.GetProfiles()) || [];
        profilesList.set(profiles);

        if (get(currentProfile) === profileName) {
            currentProfile.set("");
            authState.set('profiles');
        }
    } catch (e) {
        console.error("[AuthStore] Failed to delete identity:", e);
        alert("Failed to delete identity: " + e);
    }
};

export const bootCoreNode = async () => {
    try {
        if (window.go?.main?.App?.StartNode) {
            await window.go.main.App.StartNode();
        }
        initNetworkListeners();
        initChatListeners();
        authState.set('app');
    } catch (e) {
        console.error("[AuthStore] Failed to start node:", e);
        throw e;
    }
};