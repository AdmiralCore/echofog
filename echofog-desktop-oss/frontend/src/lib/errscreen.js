import { writable } from 'svelte/store';

// The full-screen errors (T11.5) and the notices of the runtime, by the codes the core uses.
export const SCREEN_CODES = [];
export const NOTICE_KINDS = [];

/** The error screen being shown: null or { code }. */
export const errorScreen = writable(null);

/** The i18n keys of a screen, or null for a code the app does not know. */
export function screenKeys(code) {
  return null;
}

/** The i18n key of a notice kind, or '' for a kind the app does not show. */
export function noticeKey(kind) {
  return '';
}
