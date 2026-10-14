/**
 * EchoFog Industrial Boot Service
 * Handles resilient startup, Wails bridge attachment with timeout,
 * and error-guarded initialization.
 */

export class BootService {
    /**
     * Waits for Wails runtime and bindings to attach within a timeout.
     * @param {number} timeoutMs Max wait time in ms (default 3000ms)
     * @returns {Promise<boolean>} True if attached, false if timed out
     */
    static async waitForBridge(timeoutMs = 3000) {
        const start = Date.now();
        while (Date.now() - start < timeoutMs) {
            if (window.go?.main?.App && window.runtime) {
                return true;
            }
            await new Promise((resolve) => setTimeout(resolve, 50));
        }
        return !!(window.go?.main?.App);
    }

    /**
     * Safely executes an App RPC method with bridge check.
     */
    static async callAppMethod(methodName, ...args) {
        const hasBridge = await this.waitForBridge(1500);
        if (!hasBridge || !window.go?.main?.App?.[methodName]) {
            console.warn(`[BootService] Bridge or method ${methodName} unavailable.`);
            throw new Error(`Wails bridge method "${methodName}" not ready.`);
        }
        return window.go.main.App[methodName](...args);
    }
}
