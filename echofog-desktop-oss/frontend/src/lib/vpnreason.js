// The reasons an exit refuses or cannot be used (internal/v2/exit/reason.go, T10.4), as the core
// reports them in the VPN status: the stable codes, in the order the core has them.
export const EXIT_REASONS = ['no-camouflage', 'unreachable', 'busy', 'policy', 'rate-limited', 'no-path'];

// What the exit itself says about serving us that is a problem with the exit (VPNStatus.exitState).
export const EXIT_PROBLEMS = new Set(['disabled', 'quota', 'unreachable', 'no-camouflage']);

// Refusals that are about the site, not about the exit: "dial failed" of v1 and "unreachable" of v2.
export const SITE_REASONS = ['dial failed', 'unreachable'];

export const isSiteFailure = (code) => SITE_REASONS.includes(code);

// i18n key of a reason in the status line; a code the screen does not know reads as "unknown"
const KNOWN = new Set([...EXIT_REASONS, 'denied', 'payment', 'dial failed', 'timed out', 'exit unreachable']);
export const refusalKey = (code) => 'proxy.refusal.' + (KNOWN.has(code) ? code : 'other');

// i18n key of a reason in the per-exit history (the keys have no spaces)
export const historyKey = (code) => 'proxy.hist.why.' + (KNOWN.has(code) ? code.replace(' ', '_') : 'other');
