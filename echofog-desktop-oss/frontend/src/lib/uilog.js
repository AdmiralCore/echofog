// uilog puts what goes wrong in the interface into the app's log file (App.LogUI), next to the
// core's lines, so a call or a group can be debugged from the log alone. The console still gets a
// copy for the dev tools. The Go side clips long values and limits repeats.

const bridge = () => window.go?.main?.App?.LogUI;

function plain(v) {
  if (v == null) return v;
  if (v instanceof Error || (typeof v === 'object' && 'name' in v && 'message' in v)) {
    return v.name && v.name !== 'Error' ? `${v.name}: ${v.message}` : String(v.message || v);
  }
  if (typeof v === 'object') {
    try { return JSON.stringify(v).slice(0, 400); } catch (_) { return String(v); }
  }
  return v;
}

/** uilog(level, component, msg, attrs): level is 'debug' | 'info' | 'warn' | 'error'. */
export function uilog(level, component, msg, attrs = {}) {
  const clean = {};
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v !== undefined) clean[k] = plain(v);
  }
  const out = level === 'error' ? console.error : level === 'warn' ? console.warn : console.debug;
  out(`[${component}] ${msg}`, clean);
  try {
    const p = bridge()?.(level, component, msg, clean);
    p?.catch?.(() => { /* logging must never break the caller */ });
  } catch (_) { /* no bridge in a plain browser */ }
}

export const log = {
  debug: (c, m, a) => uilog('debug', c, m, a),
  info: (c, m, a) => uilog('info', c, m, a),
  warn: (c, m, a) => uilog('warn', c, m, a),
  error: (c, m, a) => uilog('error', c, m, a),
};

/** The error part of a log line: its name and message (DOMException names matter for media). */
export const errAttrs = (e, extra = {}) => ({ err: plain(e), ...extra });

let installed = false;
/** Errors nobody caught: scripts that threw, promises nobody awaited. */
export function installGlobalErrorLog() {
  if (installed) return;
  installed = true;
  window.addEventListener('error', (ev) => {
    log.error('app', 'uncaught error', { err: ev.error || ev.message, at: `${ev.filename || ''}:${ev.lineno || 0}` });
  });
  window.addEventListener('unhandledrejection', (ev) => {
    log.error('app', 'unhandled promise rejection', { err: ev.reason });
  });
}
