// Lets the pure parts of src/lib run under `node --test` without a bundler: the one package they
// import, svelte/store, is replaced by a minimal stand-in.
import { register } from 'node:module';
register('./hooks.mjs', import.meta.url);
