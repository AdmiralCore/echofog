import './app.css';
import App from './App.svelte';

function start() {
  return new App({ target: document.getElementById('app') });
}

// `npm run dev` with `?mock` runs the UI against a fake core (see lib/dev/mock.js).
if (import.meta.env.DEV && new URLSearchParams(location.search).has('mock')) {
  import('./lib/dev/mock.js').then(start);
} else {
  start();
}
