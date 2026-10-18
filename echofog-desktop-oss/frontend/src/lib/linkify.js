// Links in messages. Only web addresses become links (http, https, and "www." which means https);
// anything else (javascript:, file:, data:, a custom scheme) stays plain text. A link always
// opens in the system browser, never in the app window.

// a run of characters that can belong to a URL, starting at a scheme or at "www."
const URL_RE = /\b(?:https?:\/\/|www\.)[^\s<>"'`]+/gi;
const TRAIL_CHAR = /[.,;:!?'")\]}>»…]$/;

/** The address a piece of text links to, or '' when it must not be a link. */
export function safeHref(raw) {
  let s = String(raw || '').trim();
  if (!s) return '';
  if (/^www\./i.test(s)) s = 'https://' + s;
  let u;
  try {
    u = new URL(s);
  } catch (_) {
    return '';
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
  if (!u.hostname || u.username || u.password) return ''; // "https://bank.com@evil.example" hides where it goes
  return u.href;
}

/** The host a link goes to, as shown to the user (punycode, so look-alike letters show). */
export function linkHost(href) {
  try {
    return new URL(href).host;
  } catch (_) {
    return '';
  }
}

// keeps balanced closing brackets ("wiki/Foo_(bar)") and drops trailing punctuation ("see x.com.")
function trimUrl(s) {
  let out = s;
  const count = (ch) => out.split(ch).length - 1;
  while (out && TRAIL_CHAR.test(out)) {
    if (out.endsWith(')') && count('(') >= count(')')) break;
    out = out.slice(0, -1);
  }
  return out;
}

/** Splits text into plain parts and links: [{ text, href? }]. */
export function linkify(text) {
  const parts = [];
  const s = String(text || '');
  let at = 0;
  for (const m of s.matchAll(URL_RE)) {
    const raw = trimUrl(m[0]);
    const href = safeHref(raw);
    if (!href) continue;
    if (m.index > at) parts.push({ text: s.slice(at, m.index) });
    parts.push({ text: raw, href });
    at = m.index + raw.length;
  }
  if (at < s.length) parts.push({ text: s.slice(at) });
  return parts;
}

/**
 * Marks the occurrences of query (case-insensitive) inside parts: each part is split further into
 * pieces { text, href?, mark? }.
 */
export function mark(parts, query) {
  const q = String(query || '').toLowerCase();
  if (!q) return parts;
  const out = [];
  for (const p of parts) {
    const low = p.text.toLowerCase();
    let at = 0;
    for (let i = low.indexOf(q); i >= 0; i = low.indexOf(q, i + q.length)) {
      if (i > at) out.push({ ...p, text: p.text.slice(at, i) });
      out.push({ ...p, text: p.text.slice(i, i + q.length), mark: true });
      at = i + q.length;
    }
    if (at < p.text.length) out.push({ ...p, text: p.text.slice(at) });
  }
  return out;
}
