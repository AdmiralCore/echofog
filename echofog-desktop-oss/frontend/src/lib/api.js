import { tr } from './i18n/index.js';
import { log } from './uilog.js';

// Thin access to the Go side. Every bound method of main.App is reachable as App.Name(...args).
export const App = new Proxy({}, {
  get(_, name) {
    return (...args) => {
      const fn = window.go?.main?.App?.[name];
      if (!fn) return Promise.reject(new Error(`${String(name)} is not available`));
      // every failed call to the core lands in the log, by method; the arguments do not (they
      // can be passwords or messages)
      return Promise.resolve(fn(...args)).catch((err) => {
        if (name !== 'LogUI') log.warn('api', 'call to the core failed', { method: String(name), err });
        throw err;
      });
    };
  },
});

export function on(event, cb) {
  if (!window.runtime?.EventsOnMultiple) return () => {};
  return window.runtime.EventsOnMultiple(event, cb, -1);
}

export function openURL(url) {
  if (window.runtime?.BrowserOpenURL) window.runtime.BrowserOpenURL(url);
  else window.open(url, '_blank', 'noopener');
}

export function errText(err, fallback = tr('shell.somethingWrong')) {
  return (err && (err.message || String(err))) || fallback;
}

// waitForBridge resolves once the Wails runtime has attached (or after the timeout).
export async function waitForBridge(timeoutMs = 3000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    if (window.go?.main?.App) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  return !!window.go?.main?.App;
}

export async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (_) {
    return false;
  }
}
