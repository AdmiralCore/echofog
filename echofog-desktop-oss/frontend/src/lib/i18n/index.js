import { writable, derived, get } from 'svelte/store';

/**
 * Interface language. `locale` is the user's choice: 'system' (follow the OS), 'en' or 'ru'; `lang`
 * is the language actually shown. Strings live in ./ns/*.js, one file per screen, each
 * `export default { en: { 'ns.key': '…' }, ru: { … } }`. Use them as `$t('ns.key', { n: 3 })`
 * in components and `tr('ns.key')` in plain JS.
 *
 * A value can be a plural form object, picked by the `n` parameter:
 *   en: { one: '{n} member', other: '{n} members' }
 *   ru: { one: '{n} участник', few: '{n} участника', many: '{n} участников' }
 * `{name}` placeholders are replaced from the parameters. A key missing in Russian falls back to
 * English, and a key missing everywhere shows the key itself, so gaps are easy to spot.
 */
export const LANGUAGES = [
  { id: 'en', label: 'English' },
  { id: 'ru', label: 'Русский' },
];
const KEY = 'echofog.locale';

const dict = { en: {}, ru: {} };
const modules = import.meta.glob('./ns/*.js', { eager: true });
for (const m of Object.values(modules)) {
  for (const l of Object.keys(dict)) Object.assign(dict[l], m.default?.[l] || {});
}

function load() {
  try {
    const v = localStorage.getItem(KEY);
    return LANGUAGES.some((l) => l.id === v) ? v : 'system';
  } catch (_) {
    return 'system';
  }
}

const systemLang = () => {
  const list = (typeof navigator !== 'undefined' && (navigator.languages || [navigator.language])) || [];
  return list.some((l) => /^(ru|be|uk|kk)\b/i.test(l || '')) ? 'ru' : 'en';
};

export const locale = writable(load());
export const lang = derived(locale, (l) => (l === 'system' ? systemLang() : l));

locale.subscribe((l) => {
  try { localStorage.setItem(KEY, l); } catch (_) { /* private mode */ }
});
lang.subscribe((l) => {
  if (typeof document !== 'undefined') document.documentElement.lang = l;
});

const rules = {};
const pluralRule = (l) => (rules[l] ||= new Intl.PluralRules(l));

function translate(l, key, params) {
  let v = dict[l]?.[key];
  if (v === undefined && l !== 'en') v = dict.en[key];
  if (v === undefined) return key;
  if (typeof v === 'object') {
    const n = Number(params?.n ?? 0);
    const form = pluralRule(l).select(n);
    v = v[form] ?? v.other ?? v.many ?? v.one ?? '';
  }
  if (params) v = v.replace(/\{(\w+)\}/g, (m, k) => (params[k] !== undefined ? String(params[k]) : m));
  return v;
}

/** `$t('key', params)` in components; re-renders when the language changes */
export const t = derived(lang, (l) => (key, params) => translate(l, key, params));

/** translate outside components (stores, services); reads the current language */
export const tr = (key, params) => translate(get(lang), key, params);

/** BCP 47 tag for Intl / toLocale* formatting */
export const intlLocale = () => (get(lang) === 'ru' ? 'ru-RU' : 'en-US');
