import { writable } from 'svelte/store';

// The full-screen errors (T11.5) and the notices of the runtime, by the codes the core uses
// (internal/v2/app/notice.go). The core also sends a Russian text; the screens and notices here
// pick their own words by code, so English users get English.
export const SCREEN_CODES = ['other-version', 'no-cover', 'free-quota-used'];
export const NOTICE_KINDS = ['update', 'no-domestic-entry', 'release-unavailable', 'no-cover', 'clock-skew'];

/** The error screen being shown: null or { code, title, text } (title and text are the core's Russian). */
export const errorScreen = writable(null);

/** A notice other than an update, shown as a banner: null or { kind, text }. */
export const notice = writable(null);

/** The i18n keys of a screen, or null for a code the app does not know. */
export function screenKeys(code) {
  if (typeof code !== 'string' || !SCREEN_CODES.includes(code)) return null;
  return { title: `errscreen.${code}.title`, text: `errscreen.${code}.text` };
}

/** The i18n key of a notice kind, or '' for a kind the app does not show. */
export function noticeKey(kind) {
  return typeof kind === 'string' && NOTICE_KINDS.includes(kind) ? `notice.${kind}` : '';
}
