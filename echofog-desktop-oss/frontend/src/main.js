import './app.css';
import './lib/stores/theme.js';
import App from './App.svelte';
import { installGlobalErrorLog } from './lib/uilog.js';

installGlobalErrorLog();

function start() {
  return new App({ target: document.getElementById('app') });
}

// `npm run dev` with `?mock` runs the UI against a fake core (see lib/dev/mock.js);
// `?mock&gallery` shows the component gallery instead. Both are dropped from a real build.
if (import.meta.env.DEV && new URLSearchParams(location.search).has('mock')) {
  if (new URLSearchParams(location.search).has('gallery')) {
    import('./lib/dev/gallery.js').then((m) => m.default(document.getElementById('app')));
  } else {
    import('./lib/dev/mock.js').then(start);
  }
} else {
  start();
}
