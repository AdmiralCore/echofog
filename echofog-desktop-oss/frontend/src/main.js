import './app.css'
import App from './App.svelte'

let app = null;

function start() {
try {
  const target = document.getElementById('app');
  if (target) {
    // Remove static HTML fallback loader once bundle is ready
    const loader = document.getElementById('initial-loader');
    if (loader) {
      loader.remove();
    }
    app = new App({ target });
  } else {
    console.error("DOM root #app not found");
  }
} catch (err) {
  console.error("Critical failure during App instantiation:", err);
  const errBox = document.getElementById('error-box');
  const msgBox = document.getElementById('loader-msg');
  if (errBox) {
    errBox.style.display = 'block';
    errBox.textContent = 'App Init Error: ' + (err.stack || err.message || err);
  }
  if (msgBox) {
    msgBox.textContent = 'Fatal Startup Error';
  }
}
}

// `vite dev` + `?mock` runs the UI against a fake Go core (see lib/dev/wails-mock.js).
if (import.meta.env.DEV && new URLSearchParams(location.search).has('mock')) {
  import('./lib/dev/wails-mock.js').then(start);
} else {
  start();
}

export default app
