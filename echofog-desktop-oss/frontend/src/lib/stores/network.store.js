import { writable } from 'svelte/store';
import { EventsOn } from '@wailsjs/runtime/runtime';
import { GetMyID, StartLocalDiscovery, StopDiscovery, ConnectToLocalPeer } from '@wailsjs/go/main/App';

export const myPeerID = writable("");
export const localPeers = writable({});

// --- EchoFog API Table Methods & Stores ---
export const networkStatus = writable({
    natType: "Unknown",
    peerCount: 0,
    ping: 0,
    status: "DISCONNECTED" // CONNECTING, ONLINE, OFFLINE
});

export const titForTatBalance = writable({
    givenUploadBytes: 0,
    receivedDownloadBytes: 0,
    netBalance: 0
});

export const incomingMessages = writable([]);

export const initNetworkListeners = async () => {
    try {
        const id = await GetMyID();
        if (id) myPeerID.set(id);
    } catch (e) {
        console.warn("Could not fetch GetMyID on init", e);
    }

    // Existing peer discovery
    EventsOn("peer_discovered", (peer) => {
        localPeers.update(current => {
            if (!current[peer.peer_id]) {
                peer.is_connected = false;
                current[peer.peer_id] = peer;
            }
            return current;
        });
    });

    // Reactive Backend event bridge (Crucial: NO POLLING)
    EventsOn("on_new_message", (data) => {
        console.log("[Network Store] New P2P message received:", data);
        incomingMessages.update(msgs => [...msgs, data]);
    });

    EventsOn("network_status_update", (status) => {
        networkStatus.set(status);
    });

    EventsOn("tit_for_tat_update", (balance) => {
        titForTatBalance.set(balance);
    });
};

export const scanNetwork = async (alias) => {
    await StartLocalDiscovery(alias);
};

export const stopScan = async () => {
    await StopDiscovery();
};

export const connectToPeer = async (peerID, addr) => {
    try {
        await ConnectToLocalPeer(addr);
        localPeers.update(peers => {
            if (peers[peerID]) {
                peers[peerID].is_connected = true;
            }
            return peers;
        });
        return true;
    } catch (err) {
        console.error("Connection failed:", err);
        return false;
    }
};

/**
 * Invokes Wails Go backend to fetch the latest immediate network status.
 */
export async function getNetworkStatus() {
    try {
        if (window.go?.main?.App?.GetNetworkStatus) {
            const status = await window.go.main.App.GetNetworkStatus();
            networkStatus.set(status);
        }
    } catch (e) {
        console.error("Failed to fetch network status:", e);
    }
}

/**
 * Commands the local AI through the Go backend.
 */
export async function commandAI(prompt) {
    const res = await window.go.main.App.CommandAI(prompt);
    if (res.error) throw new Error(res.error);
    return res.text;
}
